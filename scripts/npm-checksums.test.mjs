import assert from "node:assert/strict";
import { test } from "node:test";
import { parseChecksums } from "./npm-checksums.mjs";

const digest = (char) => char.repeat(64);
const complete = [
  `${digest("a")}  argon-darwin-amd64`,
  `${digest("b")}  argon-darwin-arm64`,
  `${digest("c")}  argon-linux-amd64`,
  `${digest("d")} *argon-linux-arm64`,
  `${digest("e")}  argon-windows-amd64.exe`,
  `${digest("f")}  release-provenance.json`,
  "",
].join("\n");

test("keeps exactly the binary digests the installer can ask for", () => {
  assert.deepEqual(parseChecksums(complete), {
    "argon-darwin-amd64": digest("a"),
    "argon-darwin-arm64": digest("b"),
    "argon-linux-amd64": digest("c"),
    "argon-linux-arm64": digest("d"),
    "argon-windows-amd64.exe": digest("e"),
  });
});

test("refuses an incomplete or malformed release manifest", () => {
  assert.throws(() => parseChecksums(complete.replace(/.*argon-linux-arm64\n/, "")), /lacks argon-linux-arm64/);
  assert.throws(() => parseChecksums(`not a checksum line\n${complete}`), /Unrecognized/);
  assert.throws(() => parseChecksums(complete.replace(digest("a"), "A".repeat(64))), /Unrecognized/);
});
