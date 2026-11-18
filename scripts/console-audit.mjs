#!/usr/bin/env node
// CI gate for npm advisories in the console's dependency tree (web/).
//
// The whole tree is audited. An advisory fails the gate unless
// scripts/console-audit-exceptions.json names it, and an exception is
// accepted only while all of these hold:
//   - a production install is clean (`npm audit --omit=dev` reports nothing),
//     so only build-time dependencies can be excused;
//   - npm offers no compatible fix (once it does, update
//     web/package-lock.json and delete the exception);
//   - the advisory is still reported (an exception nothing needs is an error).
// Anything unexpected, such as a failed registry request, also fails the gate.
//
// The exceptions file lives outside web/ because every file under web/ is
// part of the console's source hash (scripts/ui-provenance.mjs).
import { spawnSync } from "node:child_process";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";

const ADVISORY_ID = /^GHSA(?:-[0-9a-z]{4}){3}$/;
const ADVISORY_URL = /^https:\/\/github\.com\/advisories\/(GHSA(?:-[0-9a-z]{4}){3})$/;

// One entry per advisory, read from the objects in each package's `via` list.
// A string in `via` only names the dependency that passed an advisory on.
export function advisories(report) {
  const packages = report?.vulnerabilities;
  if (report?.error || typeof packages !== "object" || packages === null) {
    const detail = JSON.stringify(report?.error ?? report ?? null).slice(0, 300);
    throw new Error(`npm audit did not return a report: ${detail}`);
  }
  const found = new Map();
  for (const [name, entry] of Object.entries(packages)) {
    for (const via of entry.via ?? []) {
      if (typeof via === "string") continue;
      const id = ADVISORY_URL.exec(via.url ?? "")?.[1] ?? `npm-${via.source}`;
      const advisory = found.get(id) ?? {
        id,
        package: via.name ?? name,
        severity: via.severity,
        range: via.range,
        url: via.url,
        compatibleFix: false,
      };
      // `fixAvailable: true` means `npm audit fix` resolves it without a
      // breaking change. An object there means only a breaking change would.
      if (entry.fixAvailable === true) advisory.compatibleFix = true;
      found.set(id, advisory);
    }
  }
  return [...found.values()].sort((a, b) => a.id.localeCompare(b.id));
}

export function parseExceptions(text) {
  const entries = JSON.parse(text);
  if (!Array.isArray(entries)) throw new Error("audit exceptions must be a JSON array");
  const seen = new Set();
  for (const entry of entries) {
    const label = JSON.stringify(entry?.advisory ?? entry);
    for (const field of ["advisory", "package", "reason", "added"]) {
      if (typeof entry?.[field] !== "string" || !entry[field].trim()) {
        throw new Error(`audit exception ${label} needs "${field}"`);
      }
    }
    if (!ADVISORY_ID.test(entry.advisory)) throw new Error(`audit exception ${label} is not a GHSA ID`);
    if (!/^\d{4}-\d{2}-\d{2}$/.test(entry.added)) throw new Error(`audit exception ${label} needs "added" as YYYY-MM-DD`);
    if (seen.has(entry.advisory)) throw new Error(`audit exception ${label} is listed twice`);
    seen.add(entry.advisory);
  }
  return entries;
}

// `problems` lists every reason the gate fails; empty means it passes.
// `excused` lists the advisories an exception currently covers.
export function evaluate({ full, production, exceptions }) {
  const problems = [];
  const excused = [];
  const inProduction = advisories(production);
  for (const advisory of inProduction) {
    problems.push(
      `${advisory.id} (${advisory.package}) reaches a production install; exceptions cover build-time dependencies only: ${advisory.url}`,
    );
  }
  const productionIds = new Set(inProduction.map((advisory) => advisory.id));
  const reported = advisories(full);
  const byId = new Map(exceptions.map((entry) => [entry.advisory, entry]));
  for (const advisory of reported) {
    if (productionIds.has(advisory.id)) continue;
    const exception = byId.get(advisory.id);
    if (!exception) {
      problems.push(
        `${advisory.id} (${advisory.package} ${advisory.range}, ${advisory.severity}) has no exception: ${advisory.url}`,
      );
    } else if (exception.package !== advisory.package) {
      problems.push(`${advisory.id} is about ${advisory.package}, but its exception names ${exception.package}`);
    } else if (advisory.compatibleFix) {
      problems.push(
        `${advisory.id} (${advisory.package}) has a compatible fix now: update web/package-lock.json and delete its exception`,
      );
    } else {
      excused.push({ ...advisory, reason: exception.reason });
    }
  }
  const reportedIds = new Set(reported.map((advisory) => advisory.id));
  for (const entry of exceptions) {
    if (!reportedIds.has(entry.advisory)) {
      problems.push(`${entry.advisory} (${entry.package}) is no longer reported: delete its exception`);
    }
  }
  return { problems, excused };
}

function audit(prefix, extra) {
  const result = spawnSync("npm", ["audit", "--json", "--prefix", prefix, ...extra], {
    encoding: "utf8",
    maxBuffer: 64 * 1024 * 1024,
  });
  if (result.error) throw result.error;
  const stderr = (result.stderr || "").trim().split("\n").slice(-3).join(" | ").slice(0, 500);
  let report;
  try {
    report = JSON.parse(result.stdout);
  } catch {
    throw new Error(`npm audit printed no JSON (exit ${result.status}): ${stderr || result.stdout.trim().slice(0, 500)}`);
  }
  if (report?.error) throw new Error(`npm audit failed (exit ${result.status}): ${stderr || JSON.stringify(report.error)}`);
  return report;
}

if (process.argv[1] === fileURLToPath(import.meta.url)) {
  const prefix = fileURLToPath(new URL("../web", import.meta.url));
  const exceptionsFile = new URL("./console-audit-exceptions.json", import.meta.url);
  try {
    const exceptions = parseExceptions(readFileSync(exceptionsFile, "utf8"));
    const { problems, excused } = evaluate({
      full: audit(prefix, []),
      production: audit(prefix, ["--omit=dev"]),
      exceptions,
    });
    for (const advisory of excused) {
      console.log(`Excused in build-time dependencies: ${advisory.id} (${advisory.package} ${advisory.range}). ${advisory.reason}`);
    }
    if (problems.length) {
      for (const problem of problems) console.error(`FAIL: ${problem}`);
      process.exit(1);
    }
    console.log(`Console dependency audit passed: production install clean; advisories excused by name: ${excused.length}.`);
  } catch (error) {
    console.error(`FAIL: ${error.message}`);
    process.exit(1);
  }
}
