import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { test } from "node:test";
import { advisories, evaluate, parseExceptions } from "./console-audit.mjs";

const clean = { auditReportVersion: 2, vulnerabilities: {} };
const picomatch = {
  source: 1247710,
  name: "picomatch",
  url: "https://github.com/advisories/GHSA-fc5q-39xc-vjpf",
  severity: "high",
  range: "<=4.0.7",
};
const breakingOnly = { name: "vite", version: "6.2.7", isSemVerMajor: true };
// The shape npm 11 printed for web/ on 2026-11-17: one advisory in picomatch,
// passed on through four dependents.
const buildTime = (fixAvailable = breakingOnly) => ({
  auditReportVersion: 2,
  vulnerabilities: {
    "@vitejs/plugin-react": { severity: "high", via: ["vite"], fixAvailable },
    fdir: { severity: "high", via: ["picomatch"], fixAvailable },
    picomatch: { severity: "high", via: [picomatch], fixAvailable },
    tinyglobby: { severity: "high", via: ["fdir", "picomatch"], fixAvailable },
    vite: { severity: "high", via: ["picomatch", "tinyglobby"], fixAvailable },
  },
});
const exception = {
  advisory: "GHSA-fc5q-39xc-vjpf",
  package: "picomatch",
  reason: "No patched release exists.",
  added: "2026-11-17",
};

test("passes a clean tree that has no exceptions", () => {
  assert.deepEqual(evaluate({ full: clean, production: clean, exceptions: [] }), { problems: [], excused: [] });
});

test("reads one advisory however many dependents pass it on", () => {
  assert.deepEqual(advisories(buildTime()), [
    {
      id: "GHSA-fc5q-39xc-vjpf",
      package: "picomatch",
      severity: "high",
      range: "<=4.0.7",
      url: picomatch.url,
      compatibleFix: false,
    },
  ]);
});

test("excuses a named advisory in build-time dependencies", () => {
  const { problems, excused } = evaluate({ full: buildTime(), production: clean, exceptions: [exception] });
  assert.deepEqual(problems, []);
  assert.equal(excused.length, 1);
  assert.equal(excused[0].id, "GHSA-fc5q-39xc-vjpf");
  assert.equal(excused[0].reason, exception.reason);
});

test("fails on an advisory that has no exception", () => {
  const { problems, excused } = evaluate({ full: buildTime(), production: clean, exceptions: [] });
  assert.equal(problems.length, 1);
  assert.match(problems[0], /GHSA-fc5q-39xc-vjpf \(picomatch <=4\.0\.7, high\) has no exception/);
  assert.deepEqual(excused, []);
});

test("never excuses an advisory that a production install carries", () => {
  const { problems, excused } = evaluate({ full: buildTime(), production: buildTime(), exceptions: [exception] });
  assert.equal(problems.length, 1);
  assert.match(problems[0], /reaches a production install/);
  assert.deepEqual(excused, []);
});

test("ends an exception once npm offers a compatible fix", () => {
  const { problems, excused } = evaluate({ full: buildTime(true), production: clean, exceptions: [exception] });
  assert.equal(problems.length, 1);
  assert.match(problems[0], /has a compatible fix now/);
  assert.deepEqual(excused, []);
});

test("rejects an exception that nothing reports any more", () => {
  const { problems } = evaluate({ full: clean, production: clean, exceptions: [exception] });
  assert.deepEqual(problems, ["GHSA-fc5q-39xc-vjpf (picomatch) is no longer reported: delete its exception"]);
});

test("rejects an exception that names another package", () => {
  const { problems, excused } = evaluate({
    full: buildTime(),
    production: clean,
    exceptions: [{ ...exception, package: "vite" }],
  });
  assert.match(problems[0], /is about picomatch, but its exception names vite/);
  assert.deepEqual(excused, []);
});

test("cannot excuse an advisory that has no GHSA ID", () => {
  const unnamed = {
    vulnerabilities: {
      leftpad: { via: [{ source: 99, name: "leftpad", url: "https://example.invalid/99", severity: "low", range: "*" }] },
    },
  };
  assert.equal(advisories(unnamed)[0].id, "npm-99");
  const { problems } = evaluate({ full: unnamed, production: clean, exceptions: [] });
  assert.match(problems[0], /npm-99 \(leftpad \*, low\) has no exception/);
  assert.throws(() => parseExceptions(JSON.stringify([{ ...exception, advisory: "npm-99" }])), /not a GHSA ID/);
});

test("fails closed when npm returns something other than a report", () => {
  assert.throws(() => advisories({ error: { code: "ENOAUDIT", summary: "registry unreachable" } }), /ENOAUDIT/);
  assert.throws(() => advisories({}), /did not return a report/);
  assert.throws(() => advisories(null), /did not return a report/);
  assert.throws(() => evaluate({ full: buildTime(), production: {}, exceptions: [exception] }), /did not return a report/);
});

test("refuses malformed exceptions", () => {
  const list = (entry) => JSON.stringify([entry]);
  assert.throws(() => parseExceptions("{}"), /must be a JSON array/);
  assert.throws(() => parseExceptions(list({ ...exception, reason: " " })), /needs "reason"/);
  assert.throws(() => parseExceptions(list({ ...exception, package: undefined })), /needs "package"/);
  assert.throws(() => parseExceptions(list({ ...exception, added: "November 2026" })), /YYYY-MM-DD/);
  assert.throws(() => parseExceptions(JSON.stringify([exception, exception])), /listed twice/);
  assert.deepEqual(parseExceptions(list(exception)), [exception]);
});

test("the committed exceptions file is well formed", () => {
  const entries = parseExceptions(readFileSync(new URL("./console-audit-exceptions.json", import.meta.url), "utf8"));
  assert.ok(Array.isArray(entries));
});
