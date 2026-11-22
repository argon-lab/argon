# Security policy

## Supported versions

Security fixes are released for the latest 2.x version. Upgrade to it before
reporting; earlier 2.x releases and the 1.x line do not receive fixes.

## Reporting a vulnerability

Please do not open a public issue for a security problem.

- Email **security@argonlabs.tech**, or
- use GitHub's "Report a vulnerability" button on the repository's Security
  tab, where it is available.

Include the version (`argon --version`), how Argon is deployed (CLI, console,
API, proxy, MCP), what you observed, and steps to reproduce if you have them.
You will get an acknowledgement, and we will tell you what we find and when a
fix is released. Credit is given in the changelog unless you prefer otherwise.

## What Argon does and does not protect

Knowing the intended boundaries helps decide whether something is a
vulnerability. The details are in
[docs/OPERATIONS.md](docs/OPERATIONS.md#exposure-and-credentials).

- The console and REST API listen on loopback by default. A non-loopback
  listener requires `ARGON_API_TOKEN`; without a token the server answers only
  to loopback hostnames.
- `argon proxy` is a router, not an authentication layer. It listens on
  loopback by default and forwards every connection to the upstream MongoDB,
  which is responsible for authenticating clients.
- By default, connection strings returned for checked-out branches and
  sandboxes carry the engine's own MongoDB credentials; such a sandbox
  isolates data, not access. `argon sandbox create --credential` (REST and
  MCP: `credential: true`) issues a MongoDB user limited to that sandbox's
  database instead. Discard or a sweep revokes it; expiry alone does not.
- The anonymous demo mode (`ARGON_DEMO_MODE=1`; the hosted instance is
  paused) scopes each visitor to one project and never returns native
  connection strings.

Reports that one of these stated boundaries can be crossed are in scope, as
are crashes or data corruption reachable from the network or from untrusted
input.
