const assert = require('node:assert/strict');
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
