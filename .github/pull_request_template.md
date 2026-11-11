## What and why

<!-- What changes, and what problem it solves. Link the issue if there is one. -->

## Verification

<!-- What you ran and what you saw. Mention anything you could not test. -->

- [ ] `gofmt` clean; `go vet ./...` and `golangci-lint run ./...` in each Go module touched
- [ ] `go test -race ./...` in each Go module touched (needs a MongoDB replica set)
- [ ] `bash scripts/sync-ui.sh --check` for console changes
- [ ] Docs updated: `docs/CLI.md`, `docs/AGENTS.md`, `docs/ARCHITECTURE.md` as applicable
- [ ] `CHANGELOG.md` entry under "Unreleased" for user-visible changes

## Behaviour changes

<!-- Anything a user upgrading has to know or do. "None" is a fine answer. -->
