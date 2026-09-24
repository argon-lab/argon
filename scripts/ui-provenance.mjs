// Source archives work without .git or private companion checkouts.
// Release manifests separately record the engine commit and public UI tree.
import { createHash } from 'node:crypto';
import { readdirSync, readFileSync, writeFileSync } from 'node:fs';
import { resolve, relative, join, sep } from 'node:path';

const [sourceArg, buildArg] = process.argv.slice(2);
if (!sourceArg || !buildArg) throw new Error('usage: ui-provenance.mjs SOURCE BUILD');
const source = resolve(sourceArg);
const build = resolve(buildArg);
const ignored = new Set(['node_modules', 'dist', '.git', '.DS_Store']);
function files(root, dir = root) {
  return readdirSync(dir, { withFileTypes: true }).flatMap(entry => {
    if (ignored.has(entry.name) || entry.name.startsWith('.env')) return [];
    const path = join(dir, entry.name);
    if (entry.isSymbolicLink()) throw new Error(`symlink unsupported in console source: ${path}`);
    return entry.isDirectory() ? files(root, path) : [relative(root, path).split(sep).join('/')];
  }).sort();
}
const sha256 = bytes => createHash('sha256').update(bytes).digest('hex');
const paths = files(source);
const digest = createHash('sha256');
for (const path of paths) digest.update(path).update('\0').update(readFileSync(join(source, path))).update('\0');
const assets = Object.fromEntries(files(build).filter(path => !['provenance.json', '.source'].includes(path)).map(path => [path, sha256(readFileSync(join(build, path)))]));
const provenance = {
  schema_version: 1,
  source: 'web/',
  source_sha256: digest.digest('hex'),
  source_file_count: paths.length,
  source_hash_algorithm: 'sha256 of sorted UTF-8 relative path + NUL + file bytes + NUL; excludes node_modules, dist, .git, .env*, .DS_Store',
  package_lock_sha256: sha256(readFileSync(join(source, 'package-lock.json'))),
  assets_sha256: assets,
};
writeFileSync(join(build, 'provenance.json'), JSON.stringify(provenance, null, 2) + '\n');
