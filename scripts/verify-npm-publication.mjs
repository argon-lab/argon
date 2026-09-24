#!/usr/bin/env node
// Verify the public registry after npm accepts an upload. Publication may take
// minutes to become readable; never retry npm publish to resolve that delay.
import { execFile } from 'node:child_process';
import { createHash } from 'node:crypto';
import { mkdtemp, mkdir, readFile, rm, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';
import { parseArgs, promisify } from 'node:util';
import versionTools from './release-version.js';
import installer from '../npm/scripts/install.js';

const REGISTRY = 'https://registry.npmjs.org';
const MCP_NAME = 'io.github.argon-lab/argon';
const execute = promisify(execFile);
class VerificationError extends Error {}

export function optionsFromArgs(args) {
  const { values } = parseArgs({ args, options: {
    version: { type: 'string' },
    'timeout-ms': { type: 'string', default: '900000' },
    'interval-ms': { type: 'string', default: '30000' },
    'metadata-only': { type: 'boolean', default: false },
    'allow-prerelease': { type: 'boolean', default: false },
  } });
  const version = values.version;
  if (!version || versionTools.normalizeVersion(version) !== version || (!values['allow-prerelease'] && version.includes('-'))) {
    throw new Error('--version must be an exact stable version such as 2.1.2 (use --allow-prerelease for an exact prerelease)');
  }
  const positive = key => {
    const value = Number(values[key]);
    if (!/^\d+$/.test(values[key]) || !Number.isSafeInteger(value) || value < 1 || value > 2147483647) {
      throw new Error(`--${key} must be a positive integer no greater than 2147483647`);
    }
    return value;
  };
  return { version, timeoutMS: positive('timeout-ms'), intervalMS: positive('interval-ms'), metadataOnly: values['metadata-only'] };
}

function remaining(deadline, now) {
  const time = deadline - now();
  if (time <= 0) throw new Error('publication verification deadline reached');
  return Math.min(time, 2147483647);
}

async function response(url, deadline, { fetchImpl, now }) {
  const result = await fetchImpl(url, { signal: AbortSignal.timeout(Math.min(30000, remaining(deadline, now))), cache: 'no-store' });
  if (!result.ok) throw new Error(`HTTP ${result.status} from ${url}`);
  return result;
}

export async function verifyCleanInstall(version, deadline, deps = {}) {
  const { run = execute, fetchImpl = fetch, now = Date.now } = deps;
  const root = await mkdtemp(join(tmpdir(), 'argon-npm-verify-'));
  try {
    const prefix = join(root, 'prefix');
    const cache = join(root, 'cache');
    const userconfig = join(root, 'npmrc');
    await mkdir(prefix);
    await writeFile(userconfig, '');
    await run('npm', ['install', '--prefix', prefix, '--cache', cache,
      '--registry', REGISTRY, '--userconfig', userconfig, '--ignore-scripts=false',
      '--no-audit', '--no-fund', '--foreground-scripts', `argonctl@${version}`],
    { cwd: root, encoding: 'utf8', timeout: remaining(deadline, now), maxBuffer: 2 * 1024 * 1024 });
    const packageRoot = join(prefix, 'node_modules', 'argonctl');
    const pkg = JSON.parse(await readFile(join(packageRoot, 'package.json'), 'utf8'));
    if (pkg.name !== 'argonctl' || pkg.version !== version || pkg.mcpName !== MCP_NAME) {
      throw new VerificationError('Installed package identity/version/mcpName differs from the requested publication');
    }
    const suffix = process.platform === 'win32' ? '.exe' : '';
    const asset = `argon-${installer.getPlatform()}${suffix}`;
    const checksums = await (await response(`https://github.com/argon-lab/argon/releases/download/v${version}/SHA256SUMS`, deadline, { fetchImpl, now })).text();
    const matches = checksums.split(/\r?\n/).map(line => /^([a-fA-F0-9]{64})\s+\*?(\S+)$/.exec(line)).filter(match => match?.[2] === asset);
    if (matches.length !== 1) throw new VerificationError(`Release SHA256SUMS must contain exactly one checksum for ${asset}`);
    const sha256 = createHash('sha256').update(await readFile(join(packageRoot, 'bin', `argon-bin${suffix}`))).digest('hex');
    if (sha256 !== matches[0][1].toLowerCase()) throw new VerificationError(`Installed ${asset} failed release SHA256 verification`);
    const { stdout } = await run(process.execPath, [join(packageRoot, 'bin', 'argon.js'), '--version'],
      { cwd: root, encoding: 'utf8', timeout: Math.min(30000, remaining(deadline, now)), maxBuffer: 1024 * 1024 });
    if (stdout.trim() !== `argon version ${version}`) throw new VerificationError(`Installed CLI reported an unexpected version: ${stdout.trim()}`);
    return { asset, sha256, cliVersion: stdout.trim() };
  } finally {
    await rm(root, { recursive: true, force: true });
  }
}

export async function verifyPublication(options, deps = {}) {
  const { fetchImpl = fetch, now = Date.now, sleep = ms => new Promise(resolve => setTimeout(resolve, ms)), log = console.log } = deps;
  const deadline = now() + options.timeoutMS;
  let lastError;
  let attempt = 0;
  while (now() < deadline) {
    attempt++;
    try {
      const metadata = await (await response(`${REGISTRY}/argonctl/${options.version}`, deadline, { fetchImpl, now })).json();
      if (metadata.name !== 'argonctl' || metadata.version !== options.version || metadata.mcpName !== MCP_NAME) {
        throw new Error('Matching public package metadata and mcpName are not visible yet');
      }
      if (options.metadataOnly) {
        log(`argonctl@${options.version} is visible on the public registry with the expected mcpName.`);
        return { version: options.version, metadataOnly: true };
      }
      const installed = await verifyCleanInstall(options.version, deadline, { ...deps, fetchImpl, now });
      log(`Verified argonctl@${options.version}: fresh npm install, release SHA256 ${installed.sha256}, ${installed.cliVersion}.`);
      return { version: options.version, ...installed };
    } catch (error) {
      // Integrity/identity failures require investigation, not another download.
      if (error instanceof VerificationError) throw error;
      lastError = error;
      const timeLeft = deadline - now();
      if (timeLeft <= 0) break;
      log(`argonctl@${options.version} is not ready (attempt ${attempt}): ${error.message}. Retrying in ${Math.min(options.intervalMS, timeLeft)} ms.`);
      await sleep(Math.min(options.intervalMS, timeLeft));
    }
  }
  throw new Error(`argonctl@${options.version} was not verified within ${options.timeoutMS} ms: ${lastError?.message ?? 'deadline reached'}. Do not republish this version; check registry processing and rerun verification.`);
}

if (process.argv[1] && resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  try {
    await verifyPublication(optionsFromArgs(process.argv.slice(2)));
  } catch (error) {
    console.error(error.message);
    process.exitCode = 1;
  }
}
