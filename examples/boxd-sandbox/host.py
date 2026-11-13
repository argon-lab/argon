#!/usr/bin/env python3
"""Run agent-written code in a boxd machine against an Argon MongoDB sandbox.

The host creates an Argon sandbox, gives a boxd machine a MongoDB user that
can only reach that sandbox, runs agent.py there, and then reviews the diff
and merges or discards it. See README.md for the setup this expects.

  python3 examples/boxd-sandbox/host.py --network argon-example --region nyc
"""

import argparse
import json
import os
import secrets
import socket
import urllib.error
import urllib.parse
import urllib.request
import uuid
from pathlib import Path
from typing import Any, Callable

from boxd import Boxd, Machine
from pymongo import MongoClient

HERE = Path(__file__).resolve().parent
AGENT_DIR = "/home/boxd/agent"
# Hosts the machine may reach while it installs PyMongo, before it gets a credential.
PACKAGE_HOSTS = ("pypi.org", "files.pythonhosted.org")

Api = Callable[..., Any]


def argon_api(base_url: str) -> Api:
    """Return a function that calls Argon's REST control plane."""

    def api(method: str, path: str, body: Any = None) -> Any:
        headers = {"Content-Type": "application/json"}
        if os.environ.get("ARGON_API_TOKEN"):
            headers["Authorization"] = "Bearer " + os.environ["ARGON_API_TOKEN"]
        request = urllib.request.Request(
            base_url.rstrip("/") + "/api/v1" + path,
            data=json.dumps(body).encode() if body is not None else None,
            headers=headers,
            method=method,
        )
        try:
            with urllib.request.urlopen(request, timeout=60) as response:
                return json.load(response)
        except urllib.error.HTTPError as error:
            raise RuntimeError(f"{method} {path}: {error.read().decode()}") from error

    return api


def database_of(connection_string: str) -> str:
    """Return the database a MongoDB connection string names."""
    path = urllib.parse.urlsplit(connection_string).path.lstrip("/")
    if not path:
        raise RuntimeError("Argon returned a connection string without a database")
    return urllib.parse.unquote(path)


def run_checked(boxd: Boxd, machine_id: str, command: str, timeout: float) -> str:
    """Run a command in the machine and fail with its output if it fails."""
    result = boxd.machines.exec(machine_id, command, timeout=timeout)
    if result.exit_code != 0:
        raise RuntimeError(
            f"{command!r} exited {result.exit_code}:\n{result.stdout}{result.stderr}"
        )
    return result.stdout


def run_agent(boxd: Boxd, machine_id: str, sandbox_uri: str, main_db: str) -> str:
    """Run agent.py with the sandbox connection string in its environment.

    The connection string is written to the command's stdin and read into
    ARGON_SANDBOX_URI there, so it is not part of any command line and is not
    written to the machine's disk.
    """
    command = [
        "bash",
        "-c",
        "read -r ARGON_SANDBOX_URI && export ARGON_SANDBOX_URI"
        f" && exec {AGENT_DIR}/bin/python {AGENT_DIR}/agent.py",
    ]
    stdout, stderr = b"", b""
    with boxd.machines.stream_exec(
        machine_id, command=command, env={"ARGON_MAIN_DB": main_db}
    ) as stream:
        stream.write(sandbox_uri + "\n")
        stream.write_eof()
        for chunk in stream.iter_chunks():
            if chunk.is_stderr:
                stderr += chunk.data
            else:
                stdout += chunk.data
    if stream.exit_code != 0:
        raise RuntimeError(
            f"agent.py exited {stream.exit_code}:\n{stdout.decode()}{stderr.decode()}"
        )
    return stdout.decode()


def run(args: argparse.Namespace) -> None:
    """Create the sandboxes, run the agent, then review and merge or discard."""
    if not os.environ.get("BOXD_API_KEY"):
        raise SystemExit("set BOXD_API_KEY to a boxd API key")
    if not os.environ.get("MONGODB_URI"):
        raise SystemExit("set MONGODB_URI to the deployment Argon uses")
    api = argon_api(args.api)
    boxd = Boxd(api_key=os.environ["BOXD_API_KEY"], region=args.region)
    run_id = uuid.uuid4().hex[:8]

    # 1. The host creates a project with some data and an Argon sandbox from it.
    project = "boxd-triage-" + run_id
    api("POST", "/projects", {"name": project})
    prefix = "/projects/" + project
    main_uri = api("POST", prefix + "/branches/main/checkout", {"actor": "seed"})[
        "connection_string"
    ]
    with MongoClient(main_uri) as client:
        main = client.get_default_database()
        main.tickets.insert_many(
            [
                {
                    "_id": "T-1",
                    "status": "open",
                    "priority": "low",
                    "resolution": "duplicate",
                },
                {"_id": "T-2", "status": "open", "priority": "high"},
                {"_id": "T-3", "status": "open", "priority": "low"},
            ]
        )
    sandbox = api(
        "POST",
        prefix + "/sandboxes",
        {"name": "agent", "actor": "agent:boxd", "ttl_minutes": 60},
    )
    sandbox_db = database_of(sandbox["connection_string"])

    user, password = "argon-agent-" + run_id, secrets.token_urlsafe(24)
    # directConnection: the replica set member is named localhost:27017, which
    # the machine can't reach, so the driver must not follow the member list.
    agent_uri = "mongodb://{}:{}@{}/{}?authSource=admin&directConnection=true".format(
        urllib.parse.quote_plus(user),
        urllib.parse.quote_plus(password),
        args.mongo_host,
        sandbox_db,
    )
    admin = MongoClient(os.environ["MONGODB_URI"])
    user_created = False
    machine = None
    try:
        # 2. A MongoDB user that can read and write the sandbox database and nothing else.
        admin.admin.command(
            "createUser",
            user,
            pwd=password,
            roles=[{"role": "readWrite", "db": sandbox_db}],
        )
        user_created = True

        # 3. An isolated machine on a network shared only with the MongoDB host.
        spec = (
            Machine.spec()
            .isolated()
            .network(args.network)
            .egress(*PACKAGE_HOSTS)
            .label("app", "argon-example")
        )
        machine = boxd.machines.create(
            args.name or "argon-agent-" + run_id, spec=spec, ssh=False
        )
        print(f"boxd machine {machine.name} ({machine.id})")
        boxd.machines.wait_until_ready(machine.id)
        run_checked(
            boxd,
            machine.id,
            f"python3 -m venv {AGENT_DIR} && {AGENT_DIR}/bin/pip install -q pymongo",
            timeout=300,
        )
        # From here on the machine reaches the network it shares with MongoDB and nothing on the internet.
        boxd.machines.set_egress_allow(machine.id, ["none"])
        boxd.machines.files.upload(
            machine.id, f"{AGENT_DIR}/agent.py", (HERE / "agent.py").read_bytes()
        )

        # 4. The agent's code runs with the restricted connection string.
        output = run_agent(boxd, machine.id, agent_uri, database_of(main_uri))
        print("agent:", output.strip())
    finally:
        # 5. Delete the machine and drop the user, also when the agent failed.
        try:
            if machine is not None and not args.keep_machine:
                boxd.machines.delete(machine.id)
        finally:
            try:
                if user_created:
                    admin.admin.command("dropUser", user)
            finally:
                admin.close()

    # 6. The host reviews what the agent changed, then merges or discards it.
    diff = api("GET", prefix + "/branches/agent/diff")
    for change in diff["changes"]:
        print("change:", json.dumps(change, default=str))
    with MongoClient(main_uri) as client:
        main = client.get_default_database()
        assert main.tickets.find_one({"_id": "T-1"})["status"] == "open"
        if args.discard:
            api("DELETE", prefix + "/sandboxes/agent")
            assert main.tickets.find_one({"_id": "T-1"})["status"] == "open"
            outcome = "discarded"
        else:
            plan = api("POST", prefix + "/branches/agent/merge-preview", {})
            assert not plan["conflicts"]
            api("POST", "/merge-plans/" + plan["id"] + "/apply", {})
            api("DELETE", prefix + "/sandboxes/agent")
            assert main.tickets.find_one({"_id": "T-1"})["status"] == "closed"
            assert main.triage_notes.find_one({"_id": "triage-1"})["escalated"] == 1
            outcome = "merged"

    print(
        json.dumps(
            {
                "project": project,
                "changes": len(diff["changes"]),
                "outcome": outcome,
                "machine": machine.id,
                "machine_kept": args.keep_machine,
            },
            indent=2,
        )
    )


if __name__ == "__main__":
    parser = argparse.ArgumentParser(
        description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter
    )
    parser.add_argument("--api", default="http://127.0.0.1:1818", help="Argon REST API")
    parser.add_argument(
        "--network", required=True, help="boxd network shared with the MongoDB host"
    )
    parser.add_argument(
        "--mongo-host",
        default=socket.gethostname() + ".boxd:27017",
        help="MongoDB address as the machine sees it (default: this machine on the boxd network)",
    )
    parser.add_argument(
        "--region",
        default=os.environ.get("BOXD_REGION"),
        help="boxd region of the MongoDB host",
    )
    parser.add_argument("--name", help="name for the boxd machine")
    parser.add_argument(
        "--discard",
        action="store_true",
        help="discard the agent's changes instead of merging",
    )
    parser.add_argument(
        "--keep-machine",
        action="store_true",
        help="keep the boxd machine after the run",
    )
    run(parser.parse_args())
