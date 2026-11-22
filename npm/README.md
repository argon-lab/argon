# Argon CLI

Argon gives AI agents MongoDB sandboxes, with Git for MongoDB underneath: branch, diff, merge, time travel and undo through native MongoDB drivers.

## Installation

Use Node.js 24 LTS with npm 11.16 or newer. The installer downloads the matching release binary for macOS or Linux (x64/arm64), or Windows (x64).

```bash
npm install -g argonctl --allow-scripts=argonctl
argon --version
```

The package-scoped approval lets npm 12 run Argon's binary installer. It applies
only to this command and does not change your global script policy. See
[npm's install-script documentation](https://docs.npmjs.com/cli/v12/commands/npm-install-scripts/).

For one command without a global installation:

```bash
npx --yes --allow-scripts=argonctl argonctl --version
```

## Usage

Use MongoDB 6 or newer (a current supported patch) configured as a replica set. A standalone MongoDB server cannot provide the change streams and transactions Argon requires.

```bash
export MONGODB_URI='mongodb://localhost:27017/?replicaSet=rs0'

argon projects create my-app
argon sandbox create -p my-app --ttl 1h --watch   # prints a connection string
argon diff -p my-app -b <sandbox>                 # in another terminal

# Start the MCP server for an MCP client.
argon mcp
```

Native writes are captured by `argon console`, the API server, `argon mcp`, or a running `argon watch -p <project> -b <branch>` (`sandbox create --watch` runs it for you). See the [quick start](https://github.com/argon-lab/argon/blob/master/docs/QUICK_START.md) for a full review, merge and undo.

## Building from source

Use the Go version declared in the repository's `go.mod` files. The CLI and API are separate Go modules; run their build commands from those directories:

```bash
(cd cli && go build -o argon .)
(cd api && go run .)
```

## Documentation

See the [project documentation](https://github.com/argon-lab/argon) for setup, capture lifecycle, and supported operations.

## License

MIT
