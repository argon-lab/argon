# npm publication and recovery

Normal publication is the tested tag workflow described in [Releasing Argon](README.md).
It builds binaries in the `cli/` Go module, publishes GitHub assets and SHA256SUMS,
then publishes the stamped `argonctl` package. Do not run `npm version`
independently: `VERSION`, npm, MCP and Go companion requirements must agree.

If the npm job fails after the release assets were published:

1. Inspect the upload result. Once npm reports `+ argonctl@VERSION`, the upload
   was accepted even if registry readers still return 404. Do not rerun the
   publisher: allow propagation and run
   `node scripts/verify-npm-publication.mjs --version VERSION`. It checks every
   30 seconds for up to 15 minutes, uses a fresh install prefix and cache, checks
   the installed binary against release SHA256SUMS, and runs its CLI launcher.
2. For an upload that was not accepted, resolve the credential or registry
   failure before retrying. If manual publication is necessary, check out the exact clean release tag,
   authenticate the authorized npm publisher and run `bash scripts/publish-npm.sh`.
   This script requires an exact tag, checks/stamps shared metadata, previews the
   tarball and asks for final confirmation.
3. After verification succeeds, dispatch `publish-mcp.yml` for that same version
   or rerun only its failed publication job. Its metadata-only readiness check
   also waits up to 15 minutes. The same check is available locally with
   `node scripts/verify-npm-publication.mjs --version VERSION --metadata-only`.

Pass `--allow-prerelease` when checking an exact prerelease such as `2.2.0-rc.1`.
For short diagnostic probes, `--timeout-ms` and `--interval-ms` accept positive
integer durations. Neither verification mode publishes anything.

The installer supports macOS and Linux on amd64/arm64 and Windows amd64. Windows
arm64 is not a published binary target. The npm launchers download the matching
GitHub release asset at install time, so publishing npm before those assets exist
produces a broken installation. Prereleases use npm's `next` tag; stable releases
use `latest`.
