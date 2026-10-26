# Investigating slow PostgreSQL backups

The October 26 Kulla deployment produced 313.1 MiB of a custom-format archive in
9m45s before the unchanged ten-minute backup gate canceled it. This is compressed
output, not the volume of database rows read. The continuous progress rules out a
dump that spent that entire period waiting for its initial table locks. It does
not distinguish CPU compression, large retained tables, storage I/O or contention.

## Measured compression improvement

The previous command left compression unspecified, selecting PostgreSQL's moderate
gzip default. Use `--format=custom --compress=zstd:1` with the bundled PostgreSQL 17
client instead. This remains a complete, lossless archive, read directly by the
matching `pg_restore`; no table, pending delivery, failed payload or raw log is
excluded. Existing gzip archives remain restorable. Backup deadlines, required
off-site uploads, checksums, atomic finalization and retention are unchanged.

An opt-in reproduction is `python3 scripts/benchmark-postgres-backup.py` on a host
with Docker access and the `postgres:17-alpine` image. It creates an isolated
202 MiB database containing 200,000 JSON telemetry records and completed-job batch
copies. Three alternating measurements per compressor on October 26 gave:

| Compression | Median dump time | Archive bytes |
| --- | ---: | ---: |
| Previous gzip default | 2.409 s | 24,801,522 |
| Zstandard level 1 | 1.515 s | 17,502,687 |

Both archives were restored into fresh databases with matching row counts and
content checksums for both tables. This measures a 37% reduction in elapsed dump
time and a 29% smaller archive on that fixture, not a production duration guarantee.
The Docker backup/restore integration test additionally checks the actual archive
compression and restores its data through the existing agent path.

## Production evidence before further changes

First inspect table and TOAST sizes without scanning event payloads:

```sql
SELECT pg_size_pretty(pg_database_size(current_database())) AS database_size;
SELECT relname, pg_size_pretty(pg_table_size(relid)) AS table_and_toast,
       pg_size_pretty(pg_indexes_size(relid)) AS indexes,
       n_live_tup, n_dead_tup
FROM pg_stat_user_tables
ORDER BY pg_table_size(relid) DESC LIMIT 12;
```

Kulla retains full ingest batches in job payloads as well as raw PostgreSQL logs.
The size report must establish their actual contribution before changing retention.
Completed jobs do not themselves prove exact delivery for old releases. Do not
purge payloads, exclude tables or disable the backup gate merely to make deployment
pass. Index files contribute to database size but are rebuilt, not copied as index
files by a logical dump. Vacuum cleans obsolete versions; ordinary vacuum does not
compact all allocated storage or remove live historical job payloads.

During a reproduction, also inspect the backup container's CPU and memory with
`docker stats --no-stream` and available disk space with `df -h`. A CPU-bound dump,
a large logical data set and a disk-bound server require different remedies. The
production table-size and resource measurements remain necessary to explain the
complete ten-minute run; the local compressor benchmark alone does not establish
its full cause.
