#!/usr/bin/env bash
# Compile a consumer outside the source tree. With a version argument this
# verifies a published module without a replace directive; CI uses its checkout.
set -euo pipefail
source_root="$(cd "$(dirname "$0")/.." && pwd)"
consumer_dir="$(mktemp -d)"
trap 'rm -rf "$consumer_dir"' EXIT
cd "$consumer_dir"
go mod init example.com/argon-consumer
if [ "$#" -eq 0 ]; then
  source_version="$(tr -d '\n\r' < "$source_root/VERSION")"
  go mod edit "-require=github.com/argon-lab/argon/v2@v$source_version"
  go mod edit "-replace=github.com/argon-lab/argon/v2=$source_root"
  go mod edit "-require=github.com/argon-lab/argon/api/v2@v$source_version"
  go mod edit "-replace=github.com/argon-lab/argon/api/v2=$source_root/api"
else
  go get "github.com/argon-lab/argon/v2@$1" "github.com/argon-lab/argon/api/v2@$1"
fi
cat > main.go <<'GO'
package main

import (
    "fmt"
    "github.com/argon-lab/argon/v2/pkg/version"
    "github.com/argon-lab/argon/v2/pkg/walcli"
    "github.com/argon-lab/argon/api/v2/server"
)

func main() {
    var services *walcli.Services
    _ = services
    _ = server.NewRouterWith
    fmt.Println(version.String())
}
GO
go mod tidy
go build .
./argon-consumer
