// Runs bin/argon.js against a stub argon-bin and sends signals to the
// launcher's own PID, the way a supervisor or a Python Popen does. The
// signal must reach the binary, and the launcher must end the way the
// binary ended.
const assert = require('node:assert/strict');
const { spawn } = require('node:child_process');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const { test } = require('node:test');

const launcherSource = path.join(__dirname, '..', 'bin', 'argon.js');
const posixOnly = { skip: process.platform === 'win32' && 'POSIX signals to a PID' };

// The stub records which signal it received. On SIGINT it exits with 42 so
// the test can tell its status from the launcher's own; it leaves SIGTERM
// unhandled so it dies by that signal.
const stub = `#!/usr/bin/env node
const fs = require('fs');
const dir = process.env.STUB_DIR;
process.on('SIGINT', () => { fs.writeFileSync(dir + '/received', 'SIGINT'); process.exit(42); });
process.on('SIGHUP', () => { fs.writeFileSync(dir + '/received', 'SIGHUP'); process.exit(43); });
fs.writeFileSync(dir + '/args', JSON.stringify(process.argv.slice(2)));
fs.writeFileSync(dir + '/ready', String(process.pid));
setInterval(() => {}, 1000);
`;

function setup(t) {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'argon-launcher-'));
  t.after(() => fs.rmSync(dir, { recursive: true, force: true }));
  fs.mkdirSync(path.join(dir, 'bin'));
  fs.copyFileSync(launcherSource, path.join(dir, 'bin', 'argon.js'));
  fs.writeFileSync(path.join(dir, 'bin', 'argon-bin'), stub, { mode: 0o755 });
  return dir;
}

function start(t, dir, args) {
  const launcher = spawn(process.execPath, [path.join(dir, 'bin', 'argon.js'), ...args], {
    env: { ...process.env, STUB_DIR: dir }, stdio: 'ignore',
  });
  const exited = new Promise(resolve => launcher.on('exit', (code, signal) => resolve({ code, signal })));
  t.after(() => {
    // Never leave a stub behind, whatever the launcher did.
    try { process.kill(Number(fs.readFileSync(path.join(dir, 'ready'), 'utf8')), 'SIGKILL'); } catch {}
    try { launcher.kill('SIGKILL'); } catch {}
  });
  return { launcher, exited };
}

async function waitFor(file, ms = 10000) {
  const deadline = Date.now() + ms;
  while (!fs.existsSync(file)) {
    if (Date.now() > deadline) throw new Error(`timed out waiting for ${file}`);
    await new Promise(resolve => setTimeout(resolve, 20));
  }
}

function alive(pid) {
  try { process.kill(pid, 0); return true; } catch { return false; }
}

async function stubPid(dir) {
  await waitFor(path.join(dir, 'ready'));
  return Number(fs.readFileSync(path.join(dir, 'ready'), 'utf8'));
}

function withTimeout(promise, ms, what) {
  return Promise.race([promise, new Promise((_, reject) => setTimeout(() => reject(new Error(`timed out: ${what}`)), ms))]);
}

for (const [signal, status] of [['SIGINT', 42], ['SIGHUP', 43]]) {
  test(`forwards ${signal} sent to the launcher and exits with the binary's status`, posixOnly, async t => {
    const dir = setup(t);
    const { launcher, exited } = start(t, dir, ['source', 'follow', '-p', 'shop']);
    const pid = await stubPid(dir);
    assert.deepEqual(JSON.parse(fs.readFileSync(path.join(dir, 'args'), 'utf8')), ['source', 'follow', '-p', 'shop']);

    process.kill(launcher.pid, signal);
    const result = await withTimeout(exited, 10000, 'launcher exit');

    await waitFor(path.join(dir, 'received'), 2000).catch(() => {});
    assert.equal(fs.existsSync(path.join(dir, 'received')) && fs.readFileSync(path.join(dir, 'received'), 'utf8'), signal,
      `the binary never received ${signal}`);
    assert.deepEqual(result, { code: status, signal: null });
    assert.equal(alive(pid), false, 'the binary is still running');
  });
}

test('re-raises the signal that killed the binary', posixOnly, async t => {
  const dir = setup(t);
  const { launcher, exited } = start(t, dir, ['watch']);
  const pid = await stubPid(dir);

  process.kill(launcher.pid, 'SIGTERM');
  const result = await withTimeout(exited, 10000, 'launcher exit');

  assert.deepEqual(result, { code: null, signal: 'SIGTERM' });
  assert.equal(alive(pid), false, 'the binary is still running');
});

test('passes the binary exit status through', async t => {
  const dir = setup(t);
  const script = process.platform === 'win32' ? null : '#!/bin/sh\nexit 7\n';
  if (!script) return t.skip('stub is a shell script');
  fs.writeFileSync(path.join(dir, 'bin', 'argon-bin'), script, { mode: 0o755 });
  const { exited } = start(t, dir, []);
  assert.deepEqual(await withTimeout(exited, 10000, 'launcher exit'), { code: 7, signal: null });
});
