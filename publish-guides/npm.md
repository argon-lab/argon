# npm publication and recovery

Normal publication is the tested tag workflow described in [Releasing Argon](README.md).
It builds binaries in the `cli/` Go module, publishes GitHub assets and SHA256SUMS,
then publishes the stamped `argonctl` package. Do not run `npm version`
independently: `VERSION`, npm, MCP and Go companion requirements must agree.

If the npm job fails after the release assets were published:

1. Resolve the credential or registry failure and rerun the failed job. Verify
   whether the version already exists before retrying; npm versions are immutable.
2. If manual recovery is necessary, check out the exact clean release tag,
   authenticate the authorized npm publisher and run `bash scripts/publish-npm.sh`.
   This script requires an exact tag, checks/stamps shared metadata, previews the
   tarball and asks for final confirmation.
3. Check `npm view argonctl@VERSION version mcpName` and install into a fresh
   temporary prefix. Run both `argon --version` and `argonctl --version`.
4. If npm was recovered manually, dispatch `publish-mcp.yml` for that same version.

The installer supports macOS and Linux on amd64/arm64 and Windows amd64. Windows
arm64 is not a published binary target. The npm launchers download the matching
GitHub release asset at install time, so publishing npm before those assets exist
produces a broken installation. Prereleases use npm's `next` tag; stable releases
use `latest`.
