#!/usr/bin/env node
'use strict';

// VERSION is the source-build default. A release tag overrides it in the CI
// checkout, together with both package manifests; nothing is committed by CI.
const fs = require('node:fs');
const path = require('node:path');

function normalizeVersion(input) {
  const version = input.replace(/^v/, '');
  const number = '(0|[1-9][0-9]*)';
  const identifier = '(?:0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*)';
  if (!new RegExp(`^${number}\\.${number}\\.${number}(?:-${identifier}(?:\\.${identifier})*)?$`).test(version)) {
    throw new Error(`Expected a release version such as 2.0.1 or v2.1.0-rc.1; got ${JSON.stringify(input)}`);
  }
  return version;
}

function syncVersion(root, input, check = false) {
  const source = fs.readFileSync(path.join(root, 'VERSION'), 'utf8').trim();
  const version = normalizeVersion(input === undefined ? source : input);
  const packagePath = path.join(root, 'npm/package.json');
  const serverPath = path.join(root, 'server.json');
  const pkg = JSON.parse(fs.readFileSync(packagePath, 'utf8'));
  const server = JSON.parse(fs.readFileSync(serverPath, 'utf8'));
  const npmPackage = server.packages.find(p => p.registryType === 'npm' && p.identifier === pkg.name);
  if (!npmPackage || server.name !== pkg.mcpName) {
    throw new Error('server.json must identify the npm package and match its mcpName');
  }
  if (check) {
    if ([source, pkg.version, server.version, npmPackage.version].some(value => value !== version)) {
      throw new Error('VERSION, npm/package.json, and server.json disagree; run node scripts/release-version.js');
    }
  } else {
    pkg.version = server.version = npmPackage.version = version;
    fs.writeFileSync(path.join(root, 'VERSION'), `${version}\n`);
    fs.writeFileSync(packagePath, `${JSON.stringify(pkg, null, 2)}\n`);
    fs.writeFileSync(serverPath, `${JSON.stringify(server, null, 2)}\n`);
  }
  return version;
}

if (require.main === module) {
  try {
    const args = process.argv.slice(2);
    const check = args.includes('--check');
    const versions = args.filter(arg => arg !== '--check');
    if (versions.length > 1) throw new Error('Usage: node scripts/release-version.js [version] [--check]');
    console.log(syncVersion(path.resolve(__dirname, '..'), versions[0], check));
  } catch (error) {
    console.error(error.message);
    process.exitCode = 1;
  }
}

module.exports = { normalizeVersion, syncVersion };
