# Argon CLI

Argon creates versioned MongoDB sandboxes for branches, time travel, merges, and undo with native MongoDB drivers.

## Installation

Requires Node.js 18 or newer. The installer downloads the matching release binary for macOS or Linux (x64/arm64), or Windows (x64).

```bash
npm install -g argonctl
argon --version
```

## Usage

Use MongoDB 7 or newer configured as a replica set. A standalone MongoDB server cannot provide the change streams and transactions Argon requires.

```bash
export MONGODB_URI='mongodb://localhost:27017/?replicaSet=rs0'

argon projects create my-app
argon branches create feature-x --project my-app
argon time-travel info --project my-app --branch feature-x

# Start the MCP server for an MCP client.
argon mcp
```

Native writes require the API server, MCP server, or a running `argon ingest` process to capture history. `time-travel info` lists available history; use `time-travel query --project my-app --branch feature-x --lsn <LSN> --collection <collection>` to read a retained version.

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
