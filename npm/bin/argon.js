#!/usr/bin/env node
// Thin launcher: the real binary is downloaded by scripts/install.js at
// install time (per platform). npm requires bin targets to exist inside
// the published tarball, so the entry point is this wrapper, not the
// binary itself.

const { spawnSync } = require('child_process');
const path = require('path');
const fs = require('fs');

const binaryName = process.platform === 'win32' ? 'argon-bin.exe' : 'argon-bin';
const binaryPath = path.join(__dirname, binaryName);

if (!fs.existsSync(binaryPath)) {
  console.error('The Argon binary is missing. Reinstall with: npm install -g argonctl');
  console.error('(The installer downloads it from https://github.com/argon-lab/argon/releases)');
  process.exit(1);
}

const result = spawnSync(binaryPath, process.argv.slice(2), { stdio: 'inherit' });
process.exit(result.status === null ? 1 : result.status);
