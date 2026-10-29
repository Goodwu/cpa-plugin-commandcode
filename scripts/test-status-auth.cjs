// Read-only browser-script regression checks; no real credentials or HTTP calls.
const fs = require('node:fs');
const vm = require('node:vm');
const assert = require('node:assert/strict');
const path = require('node:path');
const html = fs.readFileSync(path.join(__dirname, '../web/status.html'), 'utf8');
const script = html.match(/<script>([\s\S]*?)<\/script>/)[1];
async function attempts(source, stored, session) {
  const nodes = new Map(), calls = [], ticks = [];
  const node = id => {
    if (!nodes.has(id)) nodes.set(id, { style: {}, classList: { add() {}, remove() {} }, focus() {}, addEventListener() {} });
    return nodes.get(id);
  };
  vm.runInNewContext(source, {
    document: { getElementById: node, addEventListener() {} },
    localStorage: { getItem: key => key === 'cli-proxy-auth' ? stored : null },
    sessionStorage: { getItem: () => session },
    fetch: async (url, options) => { calls.push(options.headers.Authorization); return { status: 401 }; },
    setInterval: fn => ticks.push(fn), clearInterval() {}, setTimeout() {}, Date,
  });
  await new Promise(setImmediate);
  for (let i = 0; i < 6; i++) { ticks.forEach(fn => fn()); await new Promise(setImmediate); }
  return calls;
}
(async () => {
  assert.deepEqual(await attempts(script, 'enc::v1::fake-storage', null), []);
  assert.deepEqual(await attempts(script, 'invalid-fixture-key', null), ['Bearer invalid-fixture-key']);
  assert.deepEqual(await attempts(script, 'stale-fixture-key', 'manual-fixture-key'), ['Bearer manual-fixture-key']);
  if (process.argv[2]) {
    const old = fs.readFileSync(process.argv[2], 'utf8').match(/<script>([\s\S]*?)<\/script>/)[1];
    const before = await attempts(old, 'enc::v1::fake-storage', null);
    assert.ok(before.length >= 5);
    console.log(`Old deployed script: ${before.length} failed requests; fixed script: 0 obfuscated-key requests`);
  }
  console.log('PASS: skips obfuscated values, stops auth retries, prefers manual key');
})().catch(error => { console.error(error); process.exitCode = 1; });
