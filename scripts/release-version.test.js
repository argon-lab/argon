const assert = require('node:assert/strict');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const { test } = require('node:test');
const { normalizeVersion, syncVersion } = require('./release-version');

test('release tags normalize and unsafe or ambiguous versions fail', () => {
  assert.equal(normalizeVersion('v2.1.0-rc.1'), '2.1.0-rc.1');
  for (const bad of ['main', 'v2', '2.01.0', '2.0.0-01', '2.0.0\nINJECT=x', '2.0.0+build']) {
    assert.throws(() => normalizeVersion(bad));
  }
});

test('tag overrides all artifacts and check rejects version drift', t => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'argon-version-'));
  t.after(() => fs.rmSync(root, { recursive: true, force: true }));
  fs.mkdirSync(path.join(root, 'npm'));
  fs.writeFileSync(path.join(root, 'VERSION'), '1.0.0\n');
  fs.writeFileSync(path.join(root, 'npm/package.json'), JSON.stringify({ name: 'argonctl', version: '1.1.0', mcpName: 'io.github.argon-lab/argon' }));
  fs.writeFileSync(path.join(root, 'server.json'), JSON.stringify({ name: 'io.github.argon-lab/argon', version: '1.0.0', packages: [{ registryType: 'npm', identifier: 'argonctl', version: '1.0.0' }] }));
  assert.throws(() => syncVersion(root, undefined, true), /disagree/);
  assert.equal(syncVersion(root, 'v2.2.0'), '2.2.0');
  assert.equal(syncVersion(root, undefined, true), '2.2.0');
  assert.throws(() => syncVersion(root, '2.3.0', true), /disagree/);
});
