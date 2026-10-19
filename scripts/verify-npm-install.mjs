#!/usr/bin/env node
import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { mkdtempSync, mkdirSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join } from "node:path";

const npmCli = process.env.NPM_CLI_PATH;
assert.ok(npmCli, "Set NPM_CLI_PATH to the selected npm installation's bin/npm-cli.js");
const npxCli = join(dirname(npmCli), "npx-cli.js");
const { version } = JSON.parse(readFileSync(new URL("../npm/package.json", import.meta.url)));
const requirement = `argonctl@${version}`;
const readme = readFileSync(new URL("../npm/README.md", import.meta.url), "utf8");
const globalArgs = readme.match(/^npm install -g (.+)$/m)?.[1].split(/\s+/);
const npxArgs = readme.match(/^npx (.+)$/m)?.[1].split(/\s+/);
assert.ok(globalArgs && npxArgs, "Expected global and npx installation commands in npm/README.md");
const pinPackage = (args) => args.map((arg) => arg === "argonctl" ? requirement : arg);
const root = mkdtempSync(join(tmpdir(), "argon-npm-install-"));
const userConfig = join(root, "user.npmrc");
const globalConfig = join(root, "global.npmrc");
writeFileSync(userConfig, "");
writeFileSync(globalConfig, "");
const env = Object.fromEntries(
  Object.entries(process.env).filter(([name]) => !/^npm_config_/i.test(name)),
);
env.npm_config_update_notifier = "false";

function run(cli, args, cwd) {
  const result = spawnSync(process.execPath, [cli, ...args], {
    cwd, env, encoding: "utf8", timeout: 180000, maxBuffer: 4 * 1024 * 1024,
  });
  assert.ifError(result.error);
  assert.equal(result.status, 0, `${args.join(" ")}\n${result.stdout}\n${result.stderr}`);
  assert.doesNotMatch(result.stderr, /unknown[^\n]*allow-scripts/i);
  return result.stdout.trim();
}

function config(cwd) {
  return [
    "--registry=https://registry.npmjs.org", `--cache=${join(cwd, "cache")}`,
    `--userconfig=${userConfig}`, `--globalconfig=${globalConfig}`,
    "--audit=false", "--fund=false",
  ];
}

function assertVersion(output) {
  assert.ok(output.split(/\s+/).includes(version), `Expected CLI ${version}: ${output}`);
}

try {
  const npmVersion = run(npmCli, ["--version"], root);
  const globalDir = join(root, "global");
  mkdirSync(globalDir);
  const prefix = join(globalDir, "prefix");
  run(npmCli, [
    "install", "-g", ...pinPackage(globalArgs),
    "--prefix", prefix, ...config(globalDir),
  ], globalDir);
  const packageDir = process.platform === "win32"
    ? join(prefix, "node_modules", "argonctl")
    : join(prefix, "lib", "node_modules", "argonctl");
  const globalVersion = run(join(packageDir, "bin", "argon.js"), ["--version"], globalDir);
  assertVersion(globalVersion);

  const npxDir = join(root, "npx");
  mkdirSync(npxDir);
  const npxVersion = run(npxCli, [
    ...config(npxDir), ...pinPackage(npxArgs),
  ], npxDir);
  assertVersion(npxVersion);
  console.log(JSON.stringify({ npm: npmVersion, package: requirement, globalVersion, npxVersion }));
} finally {
  rmSync(root, { recursive: true, force: true });
}
