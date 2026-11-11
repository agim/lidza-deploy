// Runs every browser suite against the built console: `npm run build`, then
// `npm run test:browser` (this file). It serves web/static once, runs the
// suites a few at a time, prints each suite's output when it finishes and a
// summary, and exits non-zero when any suite fails.
//
//   node tests/browser/run.cjs                 every suite
//   node tests/browser/run.cjs servers logs    only these suites
//   TEST_ONLY='console:' node tests/browser/run.cjs servers   checks whose name contains the text
//   TEST_WEB_URL=http://127.0.0.1:5173 ...     against a running console (Vite or the Go server)
//   TEST_CHROMIUM_PATH=/path/to/chrome ...     a specific Chromium (default: Playwright's)
//   TEST_JOBS=1 ...                            suites one at a time (default 3)
//
// A new feature's browser check is a case in one of these suites, never a
// standalone script (docs/console-design.md).
const { spawn } = require('node:child_process');
const fs = require('node:fs');
const path = require('node:path');
const { startStaticServer } = require('./fixtures/static-server.cjs');

const suites = [
  ['layout-spacing', 'templates, breadcrumbs, redirects and spacing of every view'],
  ['console-errors', 'every view renders without console errors or failed requests'],
  ['applications', 'index, header actions, wizard, environment, domains, data, settings'],
  ['servers', 'index, connect, agent, processes, cleanup, console'],
  ['databases', 'databases, backups, schedule, remote backups, storage'],
  ['logs', 'live build log, runtime log, copy, failed deployment'],
  ['settings', 'phone navigation, sidebar account menu, GitHub, defaults, updates, team, errors, account, help'],
  ['setup-auth', 'first-run setup, sign-in, signed-out redirect'],
];

function run(name, env) {
  return new Promise(resolve => {
    const started = Date.now();
    const child = spawn(process.execPath, [path.join(__dirname, `${name}.cjs`)], { env, stdio: ['ignore', 'pipe', 'pipe'] });
    let output = '';
    child.stdout.on('data', chunk => { output += chunk; });
    child.stderr.on('data', chunk => { output += chunk; });
    child.on('close', code => resolve({ name, code, output, seconds: (Date.now() - started) / 1000 }));
  });
}

(async () => {
  const started = Date.now();
  const wanted = process.argv.slice(2);
  const unknown = wanted.filter(name => !suites.some(([suite]) => suite === name));
  if (unknown.length) { console.error(`Unknown suite(s): ${unknown.join(', ')}. Suites: ${suites.map(([name]) => name).join(', ')}`); process.exit(2); }
  const selected = suites.filter(([name]) => !wanted.length || wanted.includes(name));
  let server;
  if (!process.env.TEST_WEB_URL) {
    const page = path.resolve(__dirname, '../../web/static/console.html');
    if (!fs.existsSync(page)) { console.error('web/static/console.html is missing: run `npm run build` first.'); process.exit(2); }
    server = await startStaticServer();
  }
  const env = { ...process.env, TEST_WEB_URL: process.env.TEST_WEB_URL || server.url };
  const jobs = Math.max(1, Number(process.env.TEST_JOBS || 3));
  console.log(`Browser suites against ${env.TEST_WEB_URL} (${selected.length} suites, ${jobs} at a time)`);
  const queue = [...selected], results = [];
  await Promise.all(Array.from({ length: Math.min(jobs, queue.length) }, async () => {
    for (let next = queue.shift(); next; next = queue.shift()) {
      const result = await run(next[0], env);
      results.push(result);
      console.log(`\n── ${result.name} ${result.code === 0 ? 'passed' : 'FAILED'} (${result.seconds.toFixed(1)}s)\n${result.output.trimEnd()}`);
    }
  }));
  await server?.close();
  const failed = results.filter(result => result.code !== 0);
  console.log('\nSummary');
  for (const [name, description] of selected) {
    const result = results.find(item => item.name === name);
    console.log(`  ${result.code === 0 ? 'pass' : 'FAIL'}  ${name.padEnd(15)} ${result.seconds.toFixed(1).padStart(6)}s  ${description}`);
  }
  console.log(`${failed.length ? `FAIL: ${failed.length} of ${results.length} suites failed` : `PASS: ${results.length} suites`} in ${((Date.now() - started) / 1000).toFixed(1)}s`);
  process.exit(failed.length ? 1 : 0);
})().catch(error => { console.error(error); process.exit(1); });
