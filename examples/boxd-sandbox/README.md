# A boxd code sandbox and an Argon data sandbox together

Agent-written code runs in a [boxd](https://boxd.sh/?ref=argon) machine and
writes to an Argon MongoDB sandbox. The host then reviews the diff and merges or
discards it. No LLM account is needed: `agent.py` stands in for the code an agent
would write.

```
data machine (host)                              agent machine (isolated)
  mongod with auth, argon console, host.py  ──▶   agent.py
  1. Argon sandbox + restricted user              2. writes through PyMongo
  3. diff, then merge or discard              ◀── network: argon-example only
```

`host.py` does this:

1. Creates an Argon project with three tickets and an Argon sandbox from it.
2. Creates a MongoDB user with `readWrite` on the sandbox's database only.
3. Creates an isolated boxd machine on the `argon-example` network, with SSH
   off, installs PyMongo, then sets the machine's egress allowlist to `none`.
4. Runs `agent.py` on the machine with the restricted connection string in its
   environment. The agent closes one ticket, escalates another, creates a
   `triage_notes` collection, and checks that it cannot read the main branch.
5. Drops the MongoDB user and deletes the boxd machine.
6. Prints the diff, then merges it, or discards it with `--discard`.

## How the machine reaches MongoDB

MongoDB, Argon and `host.py` run on one boxd machine, the data machine. The
agent's code runs on a second machine that `host.py` creates with `isolated`
and joins to the `argon-example` network. An isolated machine reaches only the
machines it shares a named network with, so it reaches the data machine at
`<data-machine>.boxd:27017` and no other machine you own. Only machines on
`argon-example` can open port 27017; boxd forwards a raw TCP port from the
internet only when you add one with `boxd machine proxy add --raw`.

The agent machine may reach PyPI while it installs PyMongo, before it has any
credential. Then `host.py` sets its egress allowlist to `none`, so the agent's
code can open connections to the data machine and to nothing on the internet.
The machine is created with SSH off.

## Credentials

A sandbox connection string from Argon carries the engine's MongoDB
credentials. It is a deployment credential, not a security boundary between
mutually untrusted agents. So the agent never gets it. `host.py` creates a user
for each run with `readWrite` on that sandbox's database only and drops it when
the run ends, also when the agent fails. The user's connection string goes to
the one command that runs `agent.py` over that command's stdin, which reads it
into the environment variable `ARGON_SANDBOX_URI`. It is not part of a command
line, and it is not written to the machine's disk or to this repository.

`readWrite` still lets the agent drop or rename collections in its own sandbox,
which Argon reports as degraded capture. See
[operations](../../docs/OPERATIONS.md#exposure-and-credentials).

`BOXD_API_KEY` and `MONGODB_URI` stay on the data machine.

## Run it

You need the [boxd CLI](https://docs.boxd.sh/?ref=argon) on your computer and
Argon 2.3.1 or newer. From 2.3.1, the collection the agent creates is captured
exactly without `argon collections prepare`.

On your computer, create the data machine on the network and an API key for
`host.py`:

```bash
boxd machine new argon-data --networks=argon-example --region nyc
boxd auth keys create argon-example     # prints the key once
boxd connect argon-data
```

On the data machine, start MongoDB as a one-node replica set with
authentication. A replica set with authentication needs a keyfile. The two
loops wait for the container to create the admin user, initiate the replica
set, and wait until it has a primary:

```bash
(umask 077; openssl rand -hex 16 > ~/mongo-admin-password)
export MONGO_ADMIN_PASSWORD="$(cat ~/mongo-admin-password)"
openssl rand -base64 756 > ~/mongo-keyfile
chmod 400 ~/mongo-keyfile && sudo chown 999:999 ~/mongo-keyfile
docker run -d --name argon-mongo -p 27017:27017 \
  -v ~/mongo-keyfile:/etc/mongo-keyfile:ro \
  -e MONGO_INITDB_ROOT_USERNAME=argon -e MONGO_INITDB_ROOT_PASSWORD="$MONGO_ADMIN_PASSWORD" \
  mongo:7 --replSet rs0 --keyFile /etc/mongo-keyfile --bind_ip_all
mongo() { docker exec argon-mongo mongosh --quiet -u argon -p "$MONGO_ADMIN_PASSWORD" --authenticationDatabase admin --eval "$1"; }
until mongo 'rs.initiate({_id:"rs0",members:[{_id:0,host:"localhost:27017"}]})' >/dev/null 2>&1; do sleep 1; done
until mongo 'db.hello().isWritablePrimary' 2>/dev/null | grep -q true; do sleep 1; done
```

The admin password stays in `~/mongo-admin-password`, readable only by you,
so a new shell can set `MONGO_ADMIN_PASSWORD` again. Install Argon, check the
deployment and start the console:

```bash
npm install -g argonctl --allow-scripts=argonctl
export MONGODB_URI="mongodb://argon:$MONGO_ADMIN_PASSWORD@localhost:27017/?replicaSet=rs0&authSource=admin"
argon doctor
argon console --no-browser > ~/argon-console.log 2>&1 &
```

Run the example:

```bash
git clone https://github.com/argon-lab/argon.git && cd argon
python3 -m venv ~/argon-example && ~/argon-example/bin/pip install 'boxd>=0.3.2' pymongo
read -rs BOXD_API_KEY && export BOXD_API_KEY   # paste the key from `boxd auth keys create`
~/argon-example/bin/python examples/boxd-sandbox/host.py --network argon-example --region nyc
```

It prints the boxd machine, the agent's output, the three changes it made and
the outcome. Add `--discard` to throw the run away instead, and
`--keep-machine` to keep the agent machine for a look around (its MongoDB user
is dropped all the same). Remove a kept machine with
`boxd machine remove <name> -y` on your computer. Set
`ARGON_API_TOKEN` when the console uses a bearer token. `--region` must be the
data machine's region, because networks do not cross regions.
