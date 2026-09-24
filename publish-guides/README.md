# Releasing Argon

`VERSION` is the source version. `scripts/release-version.js` synchronizes npm,
MCP and the nested Go modules' engine/API requirements. Run all release steps
from a clean reviewed checkout; do not change the source behind an existing tag.

## Prepare and validate

For example, to prepare 2.1.2:

```sh
node scripts/release-version.js 2.1.2
node scripts/release-version.js --check
node --test scripts/release-version.test.js npm/scripts/install.test.js
bash scripts/check-go-module.sh
```

Update the changelog, build the public console as described in the repository's
console instructions, and review the complete commit. CI must pass root, API and
CLI tests against a MongoDB replica set, the MinIO contract suite, native driver
compatibility, all three Go vet checks and reachable-vulnerability scans.

Create **both** Go module tags on that same reviewed commit and push them together:

```sh
git tag v2.1.2
git tag api/v2.1.2
git push --atomic origin v2.1.2 api/v2.1.2
```

The root tag publishes `github.com/argon-lab/argon/v2`; the prefixed tag publishes
the nested `github.com/argon-lab/argon/api/v2` module. The CLI is built from this
checkout, not distributed as a separately tagged Go module. Versions before
2.1.2 used invalid v2 module paths; consumers must update imports to `/v2`.

The tag workflow calls the complete CI workflow **on the tagged source**, checks
both tags and committed version metadata, then builds five platform binaries.
Only successful validation permits GitHub release → npm → MCP publication.
The npm step needs the configured `NPM_TOKEN`; MCP uses GitHub OIDC.

## Verify every channel

```sh
bash scripts/check-go-module.sh v2.1.2 # external consumer; no local replaces
npm view argonctl@2.1.2 version
npm install --prefix /tmp/argon-release-check argonctl@2.1.2
/tmp/argon-release-check/node_modules/.bin/argon --version
```

Verify the corresponding active version in the
[MCP Registry](https://registry.modelcontextprotocol.io/v0.1/servers/io.github.argon-lab%2Fargon/versions/latest),
then [update and test Homebrew](homebrew.md). Record the source commit, tags,
workflow URL, console source provenance and tested package versions in the release.
Hosted demo promotion has its own candidate validation and must not be inferred
from package publication.

## Recovery and other packages

- [npm recovery](npm.md): use the clean tagged checkout and existing release
  assets; do not regenerate or replace an already published version.
- MCP only: manually dispatch `publish-mcp.yml` with the already published npm
  version. It verifies npm visibility before publishing.
- Python: `argon-agents` is released from its
  [own repository](https://github.com/argon-lab/argon-agents). Verify the actual
  [PyPI version](https://pypi.org/project/argon-agents/) before recommending an
  install command; a GitHub release or green skipped-upload job is insufficient.
- The legacy `argon-mongodb` v1 package is frozen and is not the v2 SDK.
