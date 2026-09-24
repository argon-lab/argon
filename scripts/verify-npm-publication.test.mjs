import assert from 'node:assert/strict';
import { createHash } from 'node:crypto';
import { access, mkdir, writeFile } from 'node:fs/promises';
import { dirname, join } from 'node:path';
import test from 'node:test';
import installer from '../npm/scripts/install.js';
import { optionsFromArgs, verifyCleanInstall, verifyPublication } from './verify-npm-publication.mjs';

const metadata = { name: 'argonctl', version: '2.1.1', mcpName: 'io.github.argon-lab/argon' };

test('requires exact versions and bounded positive polling durations', () => {
  assert.deepEqual(optionsFromArgs(['--version', '2.1.1']), { version: '2.1.1', timeoutMS: 900000, intervalMS: 30000, metadataOnly: false });
  for (const version of ['latest', '^2.1.1', 'v2.1.1', '2.01.1', '2.1.1-rc.1', '2.1.1;echo bad', '2.1.1\n']) {
    assert.throws(() => optionsFromArgs(['--version', version]), version);
  }
  assert.equal(optionsFromArgs(['--version', '2.1.1-rc.1', '--allow-prerelease']).version, '2.1.1-rc.1');
  for (const value of ['0', '-1', '1.5', 'NaN', '2147483648']) assert.throws(() => optionsFromArgs(['--version', '2.1.1', '--timeout-ms', value]));
});

test('polls public version visibility without installing in metadata-only mode', async () => {
  let clock = 0, requests = 0;
  const result = await verifyPublication({ version: '2.1.1', timeoutMS: 100, intervalMS: 30, metadataOnly: true }, {
    now: () => clock, sleep: async ms => { clock += ms; }, log: () => {},
    fetchImpl: async url => {
      assert.equal(url, 'https://registry.npmjs.org/argonctl/2.1.1');
      return ++requests === 1 ? { ok: false, status: 404 } : { ok: true, json: async () => metadata };
    },
    run: () => assert.fail('metadata-only must not launch npm'),
  });
  assert.equal(result.version, '2.1.1');
  assert.equal(requests, 2);
});

test('unavailable publication stops at the deadline without republishing', async () => {
  let clock = 0, requests = 0;
  await assert.rejects(verifyPublication({ version: '2.1.1', timeoutMS: 65, intervalMS: 30, metadataOnly: true }, {
    now: () => clock, sleep: async ms => { clock += ms; }, log: () => {},
    fetchImpl: async () => { requests++; return { ok: false, status: 404 }; },
  }), /not verified within 65 ms.*Do not republish/);
  assert.equal(clock, 65);
  assert.equal(requests, 3);
});

async function installFixture(t, { checksumValid = true, cliVersion = '2.1.1' } = {}) {
  const binary = Buffer.from('fixture installed binary');
  const sha256 = createHash('sha256').update(binary).digest('hex');
  const asset = `argon-${installer.getPlatform()}${process.platform === 'win32' ? '.exe' : ''}`;
  let root;
  let npmCalls = 0;
  const deps = {
    now: () => 0,
    fetchImpl: async url => {
      assert.equal(url, 'https://github.com/argon-lab/argon/releases/download/v2.1.1/SHA256SUMS');
      return { ok: true, text: async () => `${checksumValid ? sha256 : '0'.repeat(64)}  ${asset}\n` };
    },
    run: async (command, args, options) => {
      root = options.cwd;
      if (command === 'npm') {
        npmCalls++;
        assert.equal(args[0], 'install');
        assert.equal(args.at(-1), 'argonctl@2.1.1');
        assert(args.includes('--ignore-scripts=false'));
        const prefix = args[args.indexOf('--prefix') + 1];
        const cache = args[args.indexOf('--cache') + 1];
        assert.equal(dirname(prefix), root);
        assert.equal(dirname(cache), root);
        await assert.rejects(access(cache));
        const pkg = join(prefix, 'node_modules', 'argonctl');
        await mkdir(join(pkg, 'bin'), { recursive: true });
        await writeFile(join(pkg, 'package.json'), JSON.stringify(metadata));
        await writeFile(join(pkg, 'bin', `argon-bin${process.platform === 'win32' ? '.exe' : ''}`), binary);
        return { stdout: 'installed' };
      }
      assert.equal(command, process.execPath);
      assert.equal(args[0], join(root, 'prefix', 'node_modules', 'argonctl', 'bin', 'argon.js'));
      assert.equal(args[1], '--version');
      return { stdout: `argon version ${cliVersion}\n` };
    },
  };
  t.after(async () => { assert.equal(npmCalls, 1); await assert.rejects(access(root)); });
  return deps;
}

test('fresh install uses its own prefix/cache, matches release SHA256 and runs the launcher', async t => {
  const result = await verifyCleanInstall('2.1.1', 10000, await installFixture(t));
  assert.equal(result.cliVersion, 'argon version 2.1.1');
  assert.match(result.sha256, /^[a-f0-9]{64}$/);
});

test('rejects an installed binary differing from release checksums and cleans its prefix', async t => {
  await assert.rejects(verifyCleanInstall('2.1.1', 10000, await installFixture(t, { checksumValid: false })), /failed release SHA256/);
});

test('rejects the installed launcher reporting a different version', async t => {
  await assert.rejects(verifyCleanInstall('2.1.1', 10000, await installFixture(t, { cliVersion: '2.1.0' })), /unexpected version/);
});
