// Serves the built console (web/static) the way the Go server does: every
// /console/... path is the one console page, everything else is a file.
// Browser checks use it when TEST_WEB_URL is not set, so they run without a
// separately started server. API calls are intercepted by the page fixtures.
// `node tests/browser/fixtures/static-server.cjs 8880` serves on a fixed port
// for the checks that expect TEST_WEB_URL=http://127.0.0.1:8880.
const http = require('node:http');
const fs = require('node:fs');
const path = require('node:path');

const root = path.resolve(__dirname, '../../../web/static');
const types = { '.html': 'text/html', '.js': 'text/javascript', '.css': 'text/css', '.woff2': 'font/woff2', '.svg': 'image/svg+xml', '.png': 'image/png', '.json': 'application/json' };

function serve(request, response) {
  let pathname = decodeURIComponent(new URL(request.url, 'http://localhost').pathname);
  if (pathname === '/' || pathname === '/console' || pathname.startsWith('/console/')) pathname = '/console.html';
  const file = path.join(root, path.normalize(pathname));
  if (!file.startsWith(root) || !fs.existsSync(file) || !fs.statSync(file).isFile()) { response.writeHead(404); response.end(); return; }
  response.writeHead(200, { 'Content-Type': types[path.extname(file)] || 'application/octet-stream', 'Cache-Control': 'no-cache' });
  fs.createReadStream(file).pipe(response);
}

function startStaticServer(port = 0) {
  const server = http.createServer(serve);
  return new Promise(resolve => server.listen(port, '127.0.0.1', () => resolve({ url: `http://127.0.0.1:${server.address().port}`, close: () => new Promise(done => server.close(done)) })));
}

module.exports = { startStaticServer };

if (require.main === module) startStaticServer(Number(process.argv[2] || 8880)).then(({ url }) => console.log(`Serving ${root} on ${url}`));
