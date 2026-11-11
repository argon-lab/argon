#!/usr/bin/env node
'use strict';

const crypto = require('node:crypto');
const fs = require('node:fs');
const path = require('node:path');
const https = require('node:https');
const { pipeline } = require('node:stream/promises');
const { execFileSync } = require('node:child_process');
const version = require('../package.json').version;

const PLATFORM_MAP = {
  'darwin-x64': 'darwin-amd64',
  'darwin-arm64': 'darwin-arm64',
  'linux-x64': 'linux-amd64',
  'linux-arm64': 'linux-arm64',
  'win32-x64': 'windows-amd64',
};

function getPlatform(platform = process.platform, arch = process.arch) {
  const key = `${platform}-${arch}`;
  if (!PLATFORM_MAP[key]) throw new Error(`Unsupported platform: ${key}`);
  return PLATFORM_MAP[key];
}

// Only a successful final HTTP response reaches the file. GitHub's CDN may
// redirect more than once; reject loops, insecure redirects, and error pages.
async function downloadBinary(url, destination, get = https.get, redirects = 5) {
  try {
    const parsed = new URL(url);
    if (parsed.protocol !== 'https:') throw new Error('Downloads must use HTTPS');
    const response = await new Promise((resolve, reject) => {
      const request = get(parsed, resolve);
      request.on('error', reject);
      request.setTimeout(30000, () => request.destroy(new Error('Download timed out')));
    });
    if ([301, 302, 303, 307, 308].includes(response.statusCode)) {
      response.resume();
      if (redirects === 0 || !response.headers.location) throw new Error('Invalid or excessive download redirects');
      return await downloadBinary(new URL(response.headers.location, parsed).href, destination, get, redirects - 1);
    }
    if (response.statusCode !== 200) {
      response.resume();
      throw new Error(`Download failed: HTTP ${response.statusCode}`);
    }
    await pipeline(response, fs.createWriteStream(destination));
  } catch (error) {
    fs.rmSync(destination, { force: true });
    throw error;
  }
}

// The release workflow writes checksums.json into the package from the
// release's SHA256SUMS before publishing. The npm registry's own integrity
// check covers this package, so the digests in it extend that check to the
// binary downloaded below: a replaced or corrupted release asset is rejected
// before it is ever executed.
function expectedDigest(assetName, file = path.join(__dirname, '..', 'checksums.json')) {
  let digests;
  try {
    digests = JSON.parse(fs.readFileSync(file, 'utf8'));
  } catch (error) {
    throw new Error(`This package carries no readable checksums.json (${error.code || error.message}); refusing to run an unverified binary`);
  }
  const digest = digests[assetName];
  if (typeof digest !== 'string' || !/^[0-9a-f]{64}$/.test(digest)) {
    throw new Error(`checksums.json has no SHA-256 for ${assetName}`);
  }
  return digest;
}

async function sha256(file) {
  const hash = crypto.createHash('sha256');
  await pipeline(fs.createReadStream(file), hash);
  return hash.digest('hex');
}

async function verifyDigest(file, assetName, expected) {
  const actual = await sha256(file);
  if (actual !== expected) {
    throw new Error(`${assetName} does not match the checksum published with this package (expected ${expected}, got ${actual})`);
  }
}

async function install() {
  const platform = getPlatform();
  const suffix = process.platform === 'win32' ? '.exe' : '';
  const assetName = `argon-${platform}${suffix}`;
  const url = `https://github.com/argon-lab/argon/releases/download/v${version}/${assetName}`;
  const binDir = path.join(__dirname, '..', 'bin');
  const binaryPath = path.join(binDir, `argon-bin${suffix}`);
  const temporaryPath = `${binaryPath}.${process.pid}.tmp${suffix}`;
  const digest = expectedDigest(assetName);
  fs.mkdirSync(binDir, { recursive: true });
  console.log(`Downloading Argon CLI v${version} for ${platform}...`);
  try {
    await downloadBinary(url, temporaryPath);
    await verifyDigest(temporaryPath, assetName, digest);
    if (process.platform !== 'win32') fs.chmodSync(temporaryPath, 0o755);
    const output = execFileSync(temporaryPath, ['--version'], { encoding: 'utf8', timeout: 30000 });
    const escaped = version.replace(/[.*+?^${}()|[\]\\]/g, '\\$&');
    if (!new RegExp(`(?:^|\\s)v?${escaped}(?:\\s|$)`).test(output)) {
      throw new Error(`Binary reported an unexpected version: ${output.trim()}`);
    }
    fs.renameSync(temporaryPath, binaryPath);
    console.log(`Argon CLI installed successfully: ${output.trim()}`);
  } finally {
    fs.rmSync(temporaryPath, { force: true });
  }
}

if (require.main === module) {
  install().catch(error => {
    console.error(`Failed to install Argon CLI: ${error.message}`);
    console.error('Manual downloads: https://github.com/argon-lab/argon/releases');
    process.exitCode = 1;
  });
}

module.exports = { downloadBinary, expectedDigest, getPlatform, verifyDigest };
