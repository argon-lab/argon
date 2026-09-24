import { execFileSync } from 'node:child_process';
import { createHash } from 'node:crypto';
import { readFileSync, readdirSync, writeFileSync } from 'node:fs';
import { join } from 'node:path';
const output = process.argv[2];
if (!output) throw new Error('usage: release-provenance.mjs DIST');
const git = (...args) => execFileSync('git', args, { encoding: 'utf8' }).trim();
const version = process.env.RELEASE_VERSION;
if (version !== readFileSync('VERSION', 'utf8').trim()) throw new Error('release version differs from VERSION');
const binaries = readdirSync(output).filter(name => /^argon-(linux|darwin|windows)-/.test(name)).sort();
if (binaries.length !== 5) throw new Error(`expected 5 platform binaries; found ${binaries.length}`);
const provenance = {
  schema_version: 1,
  version,
  engine_commit: git('rev-parse', 'HEAD'),
  console_tree: git('rev-parse', 'HEAD:web'),
  console: JSON.parse(readFileSync('api/server/ui/dist/provenance.json', 'utf8')),
  binaries_sha256: Object.fromEntries(binaries.map(name => [name, createHash('sha256').update(readFileSync(join(output, name))).digest('hex')])),
};
writeFileSync(join(output, 'release-provenance.json'), JSON.stringify(provenance, null, 2) + '\n');
