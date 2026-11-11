const assert = require('node:assert/strict');
const { spawnSync } = require('node:child_process');
const { EventEmitter } = require('node:events');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const { Readable } = require('node:stream');
const { test } = require('node:test');
const { downloadBinary, getPlatform } = require('./install');

function responses(sequence) {
  return (_url, callback) => {
    const request = new EventEmitter();
    request.setTimeout = () => request;
    const next = sequence.shift();
    assert.ok(next, 'unexpected extra HTTP request');
    process.nextTick(() => {
      const response = next.error ? new Readable({ read() { this.destroy(new Error('connection reset')); } }) : Readable.from([next.body || '']);
      response.statusCode = next.status;
      response.headers = next.location ? { location: next.location } : {};
      callback(response);
    });
    return request;
  };
}

function destination(t) {
  const directory = fs.mkdtempSync(path.join(os.tmpdir(), 'argon-installer-'));
  t.after(() => fs.rmSync(directory, { recursive: true, force: true }));
  return path.join(directory, 'binary');
}

test('follows multiple HTTPS redirects and saves only the binary body', async t => {
  const file = destination(t);
  await downloadBinary('https://example.com/asset', file, responses([
    { status: 302, location: '/cdn' }, { status: 307, location: 'https://cdn.example.com/asset' }, { status: 200, body: 'binary bytes' },
  ]));
  assert.equal(fs.readFileSync(file, 'utf8'), 'binary bytes');
});

test('redirected HTTP error is not installed as a successful binary', async t => {
  const file = destination(t);
  await assert.rejects(downloadBinary('https://example.com/asset', file, responses([
    { status: 302, location: '/missing' }, { status: 404, body: 'Not found' },
  ])), /HTTP 404/);
  assert.equal(fs.existsSync(file), false);
});

test('rejects insecure redirects, loops, and partial downloads', async t => {
  const file = destination(t);
  await assert.rejects(downloadBinary('https://example.com/asset', file, responses([{ status: 302, location: 'http://example.com/asset' }])), /HTTPS/);
  await assert.rejects(downloadBinary('https://example.com/asset', file, responses([{ status: 302, location: '/asset' }]), 0), /redirects/);
  await assert.rejects(downloadBinary('https://example.com/asset', file, responses([{ status: 200, error: true }])), /connection reset/);
  assert.equal(fs.existsSync(file), false);
});

test('platform support matches the published binary matrix', () => {
  assert.equal(getPlatform('darwin', 'arm64'), 'darwin-arm64');
  assert.equal(getPlatform('win32', 'x64'), 'windows-amd64');
  assert.throws(() => getPlatform('win32', 'arm64'), /Unsupported platform/);
});

test('missing binary reports the package-scoped installer approval command', t => {
  const launcher = destination(t);
  fs.copyFileSync(path.join(__dirname, '../bin/argon.js'), launcher);
  const result = spawnSync(process.execPath, [launcher, '--version'], { encoding: 'utf8' });
  assert.equal(result.status, 1);
  assert.match(result.stderr, /npm install -g argonctl --allow-scripts=argonctl/);
});

test('a downloaded binary must match the checksum shipped in the package', async t => {
  const { createHash } = require('node:crypto');
  const { expectedDigest, verifyDigest } = require('./install');
  const file = destination(t);
  fs.writeFileSync(file, 'release binary');
  const digest = createHash('sha256').update('release binary').digest('hex');
  const checksums = `${file}.checksums.json`;
  fs.writeFileSync(checksums, JSON.stringify({ 'argon-linux-amd64': digest, 'argon-darwin-arm64': 'not-a-digest' }));

  assert.equal(expectedDigest('argon-linux-amd64', checksums), digest);
  await verifyDigest(file, 'argon-linux-amd64', digest);

  fs.writeFileSync(file, 'something else');
  await assert.rejects(verifyDigest(file, 'argon-linux-amd64', digest), /does not match the checksum/);

  // No digest, a malformed one, or no checksums file at all: never run it.
  assert.throws(() => expectedDigest('argon-windows-amd64.exe', checksums), /no SHA-256 for argon-windows-amd64.exe/);
  assert.throws(() => expectedDigest('argon-darwin-arm64', checksums), /no SHA-256/);
  assert.throws(() => expectedDigest('argon-linux-amd64', `${file}.missing.json`), /refusing to run an unverified binary/);
});
