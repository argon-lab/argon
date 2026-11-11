#!/usr/bin/env node
// Turn a release's SHA256SUMS into npm/checksums.json, which the published
// package carries so its installer can verify the binary it downloads.
// Run by the release workflow between building the binaries and publishing
// to npm; see npm/scripts/install.js.
import { readFileSync, writeFileSync } from "node:fs";
import { fileURLToPath } from "node:url";

const REQUIRED = [
  "argon-darwin-amd64",
  "argon-darwin-arm64",
  "argon-linux-amd64",
  "argon-linux-arm64",
  "argon-windows-amd64.exe",
];

export function parseChecksums(text) {
  const digests = {};
  for (const line of text.split(/\r?\n/)) {
    if (!line.trim()) continue;
    const match = /^([0-9a-f]{64})\s+\*?(\S+)$/.exec(line);
    if (!match) throw new Error(`Unrecognized SHA256SUMS line: ${JSON.stringify(line)}`);
    if (match[2].startsWith("argon-")) digests[match[2]] = match[1];
  }
  const missing = REQUIRED.filter((name) => !digests[name]);
  if (missing.length) throw new Error(`SHA256SUMS lacks ${missing.join(", ")}`);
  return digests;
}

if (process.argv[1] === fileURLToPath(import.meta.url)) {
  const [source, target = new URL("../npm/checksums.json", import.meta.url)] = process.argv.slice(2);
  if (!source) {
    console.error("Usage: node scripts/npm-checksums.mjs <SHA256SUMS> [output]");
    process.exit(1);
  }
  try {
    const digests = parseChecksums(readFileSync(source, "utf8"));
    writeFileSync(target, `${JSON.stringify(digests, null, 2)}\n`);
    console.log(`Wrote ${Object.keys(digests).length} binary checksums`);
  } catch (error) {
    console.error(error.message);
    process.exit(1);
  }
}
