#!/usr/bin/env bash
# Run from the repository root. All MongoDB addresses are private to the Go
# runner's network namespace, including the localhost:27017 used by older tests.
set -eo pipefail

mode=${1:?usage: test-source-deployment.sh auth|rs3|versions [test command ...]}
shift
case "$mode" in
  auth)     pattern='^TestSource(Auth|TLS)' ;;
  rs3)      pattern='^TestSourceRS3' ;;
  versions) pattern='^Test(Source|Follow|Resync|WriteBack|OnlineImport|Import)' ;;
  *) echo "unknown deployment: $mode" >&2; exit 2 ;;
esac

engine_image=${ENGINE_IMAGE:-mongo:7.0.45}
source_image=${SOURCE_IMAGE:-mongo:7.0.45}
runner_image="golang:$(awk '$1 == "go" { print $2 }' go.mod)"
scratch=$(mktemp -d)
prefix="src-$mode-$$"
runner="$prefix-go"
containers=()
cleanup() {
  local status=$? i
  for ((i=${#containers[@]}-1; i>=0; i--)); do
    docker rm -fv "${containers[i]}" >/dev/null || status=1
  done
  rm -rf "$scratch" || status=1
  exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

# Record only IDs actually created by this invocation.
start() {
  local id
  id=$(docker create "$@")
  containers+=("$id")
  docker start "$id" >/dev/null
}

test_env=()
mounts=()
mongosh_tls=()
if [[ $mode == auth ]]; then
  openssl rand -base64 512 > "$scratch/keyfile"
  openssl req -x509 -newkey rsa:2048 -nodes -days 2 -subj '/CN=Source Test CA' \
    -keyout "$scratch/ca.key" -out "$scratch/ca.pem" 2>/dev/null
  openssl req -new -newkey rsa:2048 -nodes -subj '/CN=localhost' \
    -keyout "$scratch/server.key" -out "$scratch/server.csr" 2>/dev/null
  cat > "$scratch/extensions" <<'EOF'
subjectAltName=DNS:localhost,IP:127.0.0.1
extendedKeyUsage=serverAuth,clientAuth
EOF
  openssl x509 -req -in "$scratch/server.csr" -CA "$scratch/ca.pem" -CAkey "$scratch/ca.key" \
    -CAcreateserial -days 2 -extfile "$scratch/extensions" -out "$scratch/server.crt" 2>/dev/null
  cat "$scratch/server.key" "$scratch/server.crt" > "$scratch/server.pem"
  password_prefix=$(openssl rand -hex 20)
  source_password="${password_prefix}@tls"
  engine_password=$(openssl rand -hex 20)
  source_uri="mongodb://fixture:${password_prefix}%40tls@localhost:27018/?replicaSet=rs1&authSource=admin&tls=true&tlsCAFile=/certs/ca.pem"
  test_env+=(-e "ARGON_TEST_AUTH_SOURCE_MONGODB_URI=$source_uri" -e "ARGON_TEST_TLS_SOURCE_MONGODB_URI=$source_uri"
    -e "ARGON_TEST_AUTH_ENGINE_MONGODB_URI=mongodb://engine:$engine_password@localhost:27017/?replicaSet=rs0&authSource=admin")
  mounts+=(-v "$scratch/ca.pem:/certs/ca.pem:ro")
elif [[ $mode == rs3 ]]; then
  test_env+=(-e 'ARGON_TEST_RS3_SOURCE_MONGODB_URI=mongodb://localhost:27018,localhost:27019,localhost:27020/?replicaSet=rs1')
else
  source_uri='mongodb://localhost:27018/?replicaSet=rs1'
  test_env+=(-e "ARGON_TEST_SOURCE_MONGODB_URI=$source_uri" -e "TEST_MONGODB_URI=$source_uri")
fi

# A local rerun can reuse its downloaded modules and compiler output.
if [[ -n ${SRC_TEST_CACHE:-} ]]; then
  mounts+=(-v "$SRC_TEST_CACHE:/cache" -e GOMODCACHE=/cache/mod -e GOCACHE=/cache/build)
fi
# A linked worktree can point to Git metadata outside the mounted checkout.
start --name "$runner" -e GOFLAGS=-buildvcs=false -v "$PWD:/work:ro" -w /work "${mounts[@]}" "${test_env[@]}" "$runner_image" sleep infinity

mongod() {
  local name=$1 image=$2 port=$3 setname=$4
  shift 4
  local args=(--replSet "$setname" --port "$port" --bind_ip_all --wiredTigerCacheSizeGB 0.25)
  if [[ $mode == auth ]]; then
    # MongoDB requires its internal key to be readable only by its own user.
    start --name "$name" --network "container:$runner" -v "$scratch/keyfile:/fixture/keyfile:ro" \
      -v "$scratch/server.pem:/fixture/server.pem:ro" -v "$scratch/ca.pem:/fixture/ca.pem:ro" \
      --entrypoint bash "$image" -ec '
        install -o mongodb -g mongodb -m 400 /fixture/keyfile /tmp/keyfile
        install -o mongodb -g mongodb -m 400 /fixture/server.pem /tmp/server.pem
        exec docker-entrypoint.sh mongod --auth --keyFile /tmp/keyfile "$@"
      ' -- "${args[@]}" "$@"
  else
    start --name "$name" --network "container:$runner" "$image" "${args[@]}" "$@"
  fi
}

engine="$prefix-engine"
source="$prefix-source"
mongod "$engine" "$engine_image" 27017 rs0
members='[{_id:0,host:"localhost:27018"}]'
if [[ $mode == rs3 ]]; then
  for port in 27018 27019 27020; do
    mongod "$prefix-node-$port" "$source_image" "$port" rs1 --setParameter enableTestCommands=1
  done
  source="$prefix-node-27018"
  members='[27018,27019,27020].map((p,i)=>({_id:i,host:"localhost:"+p}))'
elif [[ $mode == auth ]]; then
  mongosh_tls=(--tls --tlsCAFile /fixture/ca.pem)
  mongod "$source" "$source_image" 27018 rs1 --tlsMode requireTLS \
    --tlsCertificateKeyFile /tmp/server.pem --tlsCAFile /fixture/ca.pem --tlsAllowConnectionsWithoutCertificates
else
  mongod "$source" "$source_image" 27018 rs1
fi

init_set() {
  local name=$1 port=$2 setname=$3 member_list=$4
  shift 4
  local shell=(docker exec "$name" mongosh "mongodb://localhost:$port/admin?directConnection=true" "$@" --quiet --eval)
  for ((i=0; i<60; i++)); do
    if "${shell[@]}" 'db.hello().ok' >/dev/null 2>&1; then break; fi
    sleep 1
  done
  "${shell[@]}" "const r=rs.initiate({_id:'$setname',members:$member_list}); if (!r.ok) throw Error(r.errmsg)"
  # Let replica-set discovery select the elected member.
  docker exec "$name" mongosh "mongodb://localhost:$port/admin?replicaSet=$setname&serverSelectionTimeoutMS=90000" \
    "$@" --quiet --eval 'if (!db.hello().isWritablePrimary) throw Error("no writable primary"); print("MongoDB "+db.version())'
}
init_set "$engine" 27017 rs0 '[{_id:0,host:"localhost:27017"}]'
init_set "$source" 27018 rs1 "$members" "${mongosh_tls[@]}"

if [[ $mode == rs3 ]]; then
  docker exec "$source" mongosh --quiet --port 27018 --eval '
    for (let i=0; i<180; i++) {
      if (rs.status().members.filter(m=>m.stateStr==="SECONDARY").length===2) quit(0)
      sleep(500)
    }
    throw Error("both source secondaries must finish initial sync before stepdown")
  '
elif [[ $mode == auth ]]; then
  # Use the localhost exception once on each fresh deployment. The restricted
  # engine user gets no user/role management privileges.
  docker exec -e "PASSWORD=$source_password" "$source" mongosh --quiet --port 27018 "${mongosh_tls[@]}" admin --eval \
    'db.createUser({user:"fixture",pwd:process.env.PASSWORD,roles:["root"]})' >/dev/null
  docker exec -e "PASSWORD=$engine_password" "$engine" mongosh --quiet --port 27017 admin --eval \
    'db.createUser({user:"engine",pwd:process.env.PASSWORD,roles:["readWriteAnyDatabase","dbAdminAnyDatabase"]})' >/dev/null
fi

echo "Ready: $runner ($mode)"
if [[ $# == 0 ]]; then
  set -- go test -race -v -count=1 -timeout 45m -run "$pattern" ./tests/wal
fi
docker exec "$runner" "$@"
if [[ $mode == auth ]]; then
  # Both the clear and URI-escaped password contain this random prefix.
  # Report a leak without copying a matching log line to CI output.
  docker logs "$source" 2>&1 | awk -v secret="$password_prefix" '
    index($0,secret) { leaked=1 }
    END { if (leaked) { print "source log contains the fixture password" > "/dev/stderr"; exit 1 } }
  '
fi
