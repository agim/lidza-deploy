#!/usr/bin/env python3
"""Opt-in Docker benchmark: full telemetry-shaped dumps and exact-content restores.

Uses only a newly created, private PostgreSQL 17 fixture; no production URL,
published port or persistent volume. Run with access to the local Docker daemon.
Reports timings rather than asserting a machine-dependent speed threshold.
"""
import json, pathlib, statistics, subprocess, tempfile, time, uuid

scratch = tempfile.TemporaryDirectory(prefix='lidza-backup-benchmark-')
root = pathlib.Path(scratch.name)
container = 'lidza-backup-compression-fixture-' + uuid.uuid4().hex[:12]

def run(*args, **kwargs):
    return subprocess.run(args, check=True, **kwargs)

def sql(query, database='fixture'):
    return run('docker','exec','-i',container,'psql','-X','-U','postgres','-d',database,'-v','ON_ERROR_STOP=1','-At',input=query.encode(),stdout=subprocess.PIPE).stdout.decode().strip()

run('docker','run','--rm','-d','--name',container,'--memory','2g','--tmpfs','/var/lib/postgresql/data:rw,size=1g','-e','POSTGRES_HOST_AUTH_METHOD=trust','postgres:17-alpine',stdout=subprocess.DEVNULL)
try:
    for attempt in range(100):
        ready = subprocess.run(['docker','exec',container,'pg_isready','-h','127.0.0.1','-U','postgres'],stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL)
        if ready.returncode == 0:
            break
        time.sleep(.1)
    sql('CREATE DATABASE fixture;', 'postgres')
    sql("""
    CREATE TABLE kulla_logs AS
    SELECT i AS id, jsonb_build_object('id',md5(i::text),'app','vat','stream','job',
      'ts',now()-i*interval '1 second','attrs',jsonb_build_object('class','SteadyJob',
      'result','ok','job_id',md5((i+1)::text),'message',repeat(md5((i%1000)::text),16),
      'duration_ms',i%10000,'path','/api/listings/'||(i%100)::text)) AS payload
    FROM generate_series(1,200000) i;
    CREATE TABLE job AS
    SELECT id, 'done'::text AS state, jsonb_build_object('id',id,'rows',jsonb_agg(payload)) AS payload
    FROM (SELECT (id/250)::int AS id,payload FROM kulla_logs) p GROUP BY id;
    ANALYZE;
    """
    )
    proof_query = "SELECT 'logs',count(*),sum(hashtextextended(payload::text,0)::numeric) FROM kulla_logs UNION ALL SELECT 'jobs',count(*),sum(hashtextextended(payload::text,0)::numeric) FROM job ORDER BY 1"
    expected = sql(proof_query)
    print('source_bytes='+sql("SELECT pg_database_size(current_database())"), flush=True)
    durations = {'gzip':[], 'zstd':[]}
    for method in ['gzip','zstd','zstd','gzip','gzip','zstd']:
        path = root / (method+'.dump')
        args = ['docker','exec',container,'pg_dump','-U','postgres','--format=custom','--no-owner','--no-acl','--lock-wait-timeout=30s']
        if method == 'zstd':
            args.append('--compress=zstd:1')
        args.append('fixture')
        start=time.monotonic()
        with path.open('wb') as out:
            run(*args,stdout=out)
        elapsed=time.monotonic()-start
        durations[method].append(elapsed)
        print(json.dumps({'method':method,'seconds':round(elapsed,3),'bytes':path.stat().st_size}),flush=True)
    for method in durations:
        sql('CREATE DATABASE restored;', 'postgres')
        with (root/(method+'.dump')).open('rb') as source:
            run('docker','exec','-i',container,'pg_restore','-U','postgres','--exit-on-error','--no-owner','--no-acl','-d','restored',stdin=source)
        assert sql(proof_query,'restored') == expected, method+' restored rows or content differ'
        print(method+' restore_exact_content=PASS',flush=True)
        sql('DROP DATABASE restored;', 'postgres')
    print(json.dumps({'median_seconds':{k:round(statistics.median(v),3) for k,v in durations.items()}}),flush=True)
finally:
    subprocess.run(['docker','rm','-f',container],stdout=subprocess.DEVNULL)
    scratch.cleanup()
