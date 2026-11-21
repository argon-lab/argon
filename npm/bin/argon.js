#!/usr/bin/env node
// Thin launcher: the real binary is downloaded by scripts/install.js at
// install time (per platform). npm requires bin targets to exist inside
// the published tarball, so the entry point is this wrapper, not the
// binary itself.

const { spawn } = require('child_process');
const path = require('path');
const fs = require('fs');

const binaryName = process.platform === 'win32' ? 'argon-bin.exe' : 'argon-bin';
const binaryPath = path.join(__dirname, binaryName);

if (!fs.existsSync(binaryPath)) {
  console.error('The Argon binary is missing. Reinstall with: npm install -g argonctl --allow-scripts=argonctl');
  console.error('(The installer downloads it from https://github.com/argon-lab/argon/releases)');
  process.exit(1);
}

const child = spawn(binaryPath, process.argv.slice(2), { stdio: 'inherit' });

// A supervisor or a script may signal only this launcher's PID, not the
// whole process group the way a terminal's Ctrl-C does. Pass those signals
// on, so a long-running command such as `argon source follow` stops cleanly
// and releases its source lease instead of running on as an orphan.
const forwarded = ['SIGINT', 'SIGTERM', 'SIGHUP'];
const handlers = {};
for (const signal of forwarded) {
  handlers[signal] = () => {
    // On Windows, Ctrl-C already reaches the binary through the shared
    // console, and child.kill() would terminate it without a clean stop.
    if (process.platform === 'win32' && signal === 'SIGINT') return;
    if (child.exitCode === null && child.signalCode === null) {
      try { child.kill(signal); } catch (_) { /* the binary already exited */ }
    }
  };
  process.on(signal, handlers[signal]);
}

child.on('error', err => {
  console.error(`Could not start the Argon binary: ${err.message}`);
  process.exit(1);
});

child.on('exit', (code, signal) => {
  if (signal && process.platform !== 'win32') {
    // End the same way the binary did, so the parent sees the signal death.
    for (const name of forwarded) process.removeListener(name, handlers[name]);
    process.kill(process.pid, signal);
    // Not reached when the signal's default action is to terminate.
    setTimeout(() => process.exit(1), 1000);
    return;
  }
  process.exit(code === null ? 1 : code);
});
