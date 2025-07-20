# Argon CLI

The official CLI for Argon - MongoDB branching with time travel.

## Installation

```bash
npm install -g argonctl
```

## Usage

```bash
# Enable time travel
export ENABLE_WAL=true

# Create project
argon projects create my-app

# Create instant branch
argon branches create feature-x

# Time travel queries
argon time-travel info --time "1h ago"
```

## Documentation

Full documentation: https://github.com/argon-lab/argon

## License

MIT