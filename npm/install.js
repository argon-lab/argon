#!/usr/bin/env node

const fs = require('fs');
const path = require('path');
const https = require('https');
const { execSync } = require('child_process');

const GITHUB_REPO = 'argon-lab/argon';
const VERSION = '1.0.0'; // Use fixed version since that's where binaries are

function getPlatform() {
  const platform = process.platform;
  const arch = process.arch;
  
  let platformName;
  switch (platform) {
    case 'darwin':
      platformName = 'darwin';
      break;
    case 'linux':
      platformName = 'linux';
      break;
    case 'win32':
      platformName = 'windows';
      break;
    default:
      throw new Error(`Unsupported platform: ${platform}`);
  }
  
  let archName;
  switch (arch) {
    case 'x64':
      archName = 'amd64';
      break;
    case 'arm64':
      archName = 'arm64';
      break;
    default:
      throw new Error(`Unsupported architecture: ${arch}`);
  }
  
  return `${platformName}-${archName}`;
}

function downloadBinary() {
  const platform = getPlatform();
  const binaryName = platform === 'windows-amd64' ? 'argon.exe' : 'argon';
  const downloadUrl = `https://github.com/${GITHUB_REPO}/releases/download/v${VERSION}/argon-${platform}${platform.includes('windows') ? '.exe' : ''}`;
  
  const binDir = path.join(__dirname, 'bin');
  const binaryPath = path.join(binDir, binaryName);
  
  // Create bin directory
  if (!fs.existsSync(binDir)) {
    fs.mkdirSync(binDir, { recursive: true });
  }
  
  console.log(`Downloading Argon CLI for ${platform}...`);
  console.log(`URL: ${downloadUrl}`);
  
  return new Promise((resolve, reject) => {
    const file = fs.createWriteStream(binaryPath);
    
    function handleResponse(response) {
      // Handle redirects
      if (response.statusCode === 302 || response.statusCode === 301) {
        const redirectUrl = response.headers.location;
        console.log(`Following redirect to: ${redirectUrl}`);
        const url = require('url');
        const parsed = url.parse(redirectUrl);
        const client = parsed.protocol === 'https:' ? https : require('http');
        
        client.get(redirectUrl, handleResponse).on('error', (err) => {
          fs.unlink(binaryPath, () => {}); // Delete partial file
          reject(err);
        });
        return;
      }
      
      if (response.statusCode !== 200) {
        reject(new Error(`Failed to download binary: HTTP ${response.statusCode}`));
        return;
      }
      
      response.pipe(file);
      
      file.on('finish', () => {
        file.close();
        
        // Make binary executable on Unix systems
        if (process.platform !== 'win32') {
          try {
            fs.chmodSync(binaryPath, '755');
          } catch (err) {
            console.warn('Warning: Could not make binary executable:', err.message);
          }
        }
        
        console.log('Argon CLI installed successfully!');
        console.log('Run "argon --version" to verify installation.');
        resolve();
      });
    }
    
    https.get(downloadUrl, handleResponse).on('error', (err) => {
      fs.unlink(binaryPath, () => {}); // Delete partial file
      reject(err);
    });
  });
}

// Install binary
downloadBinary().catch((err) => {
  console.error('Installation failed:', err.message);
  console.error('Please install manually from: https://github.com/argon-lab/argon/releases');
  process.exit(1);
});