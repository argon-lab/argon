# Argon Console SPA

The REST engine serves this build as its embedded console. Use Node.js 22.12+.

```bash
npm ci
npm audit
npm run build
```

For the real browser regression, run this engine in demo mode with a MongoDB7 replica set. Its metadata must advertise native connections disabled. Use an isolated `ARGON_METADATA_DB`; the suite creates disposable demo sessions.

```bash
# Engine API terminal (from its api/ module):
ARGON_DEMO_MODE=1 PORT=18081 ARGON_METADATA_DB=argon_ui_tests ARGON_CORS_ORIGINS=http://127.0.0.1:15173 go run .
# SPA terminal (from web/):
ARGON_API_ORIGIN=http://127.0.0.1:18081 npm run dev -- --host 127.0.0.1 --port 15173
# Test terminal:
npx playwright install chromium
npm run test:browser
```

`CONSOLE_URL` defaults to `http://127.0.0.1:15173`. The test drives a same-pin planner/executor scenario, conflict review with an explicit target-wins merge, actor undo and an assertion that the document price is restored to 49. It checks desktop/mobile overflow, no native endpoint calls or URIs in demo mode, and opt-in enum-only completion events. Screenshots go to `/tmp/argon-console-checks` (`ARTIFACT_DIR` override).

For native capture health, run the SPA against a non-demo local API and inspect a checked-out branch. The page shows lifecycle state, actor, errors, last checkpoint and the last observed capture delay. The delay is not advertised as current queue lag. A branch database can be live while capture is stopped; the UI distinguishes them.

From the repository root, run `bash scripts/sync-ui.sh` to regenerate the embedded assets in `api/server/ui/dist`. Run `bash scripts/sync-ui.sh --check` to verify they match the source and lockfile. Node.js 24 is used by CI. The generated `provenance.json` records the source and asset SHA-256 digests; GitHub releases also record the engine commit and UI tree. Commit both source changes and rebuilt assets.

PRs and pushes run the browser regression automatically against the reviewed
engine commit in the workflow, using the embedded production build. Undo regression checks include scope edits, late
preview responses and the bounded reviewed execution range. Session coverage
includes cookie loss, an accelerated expiry timestamp, fresh project creation
and old deep-link recovery. The engine and all recovered projects remain real;
only the expiry timestamp/failure UI fixtures are intercepted.

This `web/` directory is the canonical console source under the included MIT
license. A clean public clone or release source archive contains everything
needed to rebuild it. No private Cloud repository is required.
