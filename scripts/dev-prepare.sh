#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
source scripts/env.sh
mkdir -p .local bin
chmod 700 .local
python3 - <<'PY'
import json, pathlib, secrets, shlex
root=pathlib.Path.cwd(); local=root/'.local'
p=local/'dev.env'
if not p.exists():
 values={
 'LIDZA_AGENT_TOKEN':secrets.token_hex(32),'LIDZA_MASTER_KEY':secrets.token_hex(32),'AUTH_SECRET':secrets.token_hex(32),
 'CONTROL_USER':'developer@example.com','CONTROL_PASSWORD':secrets.token_urlsafe(24),
 'PUBLIC_URL':'http://127.0.0.1:3000','APP_URL':'http://127.0.0.1:3000','LIDZA_ADDR':'127.0.0.1:3000',
 'AUTH_COOKIE_SECURE':'false','DATABASE_URL':'postgres://postgres@127.0.0.1:55432/lidza_deploy?sslmode=disable',
 'CONTROL_DATA_DIR':str(local/'control'),'CONTROL_SERVERS_FILE':str(local/'servers.json')}
 with p.open('x') as f:
  for k,v in values.items(): f.write('export '+k+'='+shlex.quote(v)+'\n')
 p.chmod(0o600)
else:
 values={}
 for line in p.read_text().splitlines():
  entry=shlex.split(line.removeprefix('export '))[0];k,v=entry.split('=',1);values[k]=v
for name,data in [('agent.json',{'listen':'127.0.0.1:9090','proxy_listen':'127.0.0.1:8081','data_dir':str(local/'agent')}),('servers.json',[{'id':'local','name':'Local development','url':'http://127.0.0.1:9090','token':values['LIDZA_AGENT_TOKEN']}])]:
 p=local/name
 if not p.exists():
  with p.open('x') as f: json.dump(data,f)
  p.chmod(0o600)
PY
if docker container inspect lidza-deploy-postgres >/dev/null 2>&1; then
 docker start lidza-deploy-postgres >/dev/null
else
 docker run -d --name lidza-deploy-postgres -e POSTGRES_HOST_AUTH_METHOD=trust -e POSTGRES_DB=lidza_deploy -p 127.0.0.1:55432:5432 postgres:17-alpine >/dev/null
fi
for attempt in {1..30}; do
 if docker exec lidza-deploy-postgres pg_isready -U postgres -d lidza_deploy >/dev/null; then break; fi
 if [ "$attempt" = 30 ]; then exit 1; fi
 sleep 1
done
go mod download
go build -o bin/lidza-agent ./cmd/agent
go build -o bin/lidza-control ./cmd/web
printf '%s\n' 'Prepared. Start scripts/dev-agent.sh and scripts/dev-web.sh in separate processes.' 'Local operator credentials are in the private .local/dev.env file.'
