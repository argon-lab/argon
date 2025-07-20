#!/usr/bin/env node

const fs = require('fs');
const path = require('path');
const https = require('https');
const { execSync } = require('child_process');

const version = require('../package.json').version;

// Platform mapping
const PLATFORM_MAP = {
  'darwin-x64': 'darwin-amd64',
  'darwin-arm64': 'darwin-arm64',
  'linux-x64': 'linux-amd64',
  'linux-arm64': 'linux-arm64',
  'win32-x64': 'windows-amd64',
};

function getPlatform() {
  const platform = `${process.platform}-${process.arch}`;
  if (!PLATFORM_MAP[platform]) {
    throw new Error(`Unsupported platform: ${platform}`);
  }
  return PLATFORM_MAP[platform];
}

function getBinaryName() {
  return process.platform === 'win32' ? 'argon.exe' : 'argon';
}

function downloadBinary(url, dest) {
  return new Promise((resolve, reject) => {
    const file = fs.createWriteStream(dest);
    https.get(url, (response) => {
      if (response.statusCode === 302 || response.statusCode === 301) {
        // Follow redirect
        https.get(response.headers.location, (redirectResponse) => {
          redirectResponse.pipe(file);
          file.on('finish', () => {
            file.close(resolve);
          });
        }).on('error', reject);
      } else if (response.statusCode === 200) {
        response.pipe(file);
        file.on('finish', () => {
          file.close(resolve);
        });
      } else {
        reject(new Error(`Failed to download: ${response.statusCode}`));
      }
    }).on('error', reject);
  });
}

async function install() {
  try {
    const platform = getPlatform();
    const binaryName = getBinaryName();
    const downloadUrl = `https://github.com/argon-lab/argon/releases/download/v${version}/argon-${platform}`;
    
    const binDir = path.join(__dirname, '..', 'bin');
    const binaryPath = path.join(binDir, binaryName);

    // Create bin directory
    if (!fs.existsSync(binDir)) {
      fs.mkdirSync(binDir, { recursive: true });
    }

    console.log(`Downloading Argon CLI v${version} for ${platform}...`);
    console.log(`From: ${downloadUrl}`);
    
    await downloadBinary(downloadUrl, binaryPath);
    
    // Make binary executable on Unix-like systems
    if (process.platform !== 'win32') {
      fs.chmodSync(binaryPath, '755');
    }

    // Verify installation
    try {
      const output = execSync(`"${binaryPath}" --version`, { encoding: 'utf8' });
      console.log('✅ Argon CLI installed successfully!');
      console.log(output.trim());
    } catch (e) {
      console.error('⚠️  Binary downloaded but verification failed');
      console.error('Please check if the binary is working correctly');
    }

  } catch (error) {
    console.error('Failed to install Argon CLI:', error.message);
    console.error('\nYou can manually download from:');
    console.error('https://github.com/argon-lab/argon/releases');
    process.exit(1);
  }
}

// Run installation
install();