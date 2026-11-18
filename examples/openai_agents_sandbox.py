#!/usr/bin/env python3
"""An OpenAI Agents SDK agent that works in a disposable Argon sandbox.

The agent's task is deliberately small: give one account a 10% discount.
The example is about the data workflow around it:

  1. seed an ``accounts`` collection on the parent branch (``main``),
  2. fork a sandbox with ``argon-agents`` and let the agent write to it
     with ``pymongo``,
  3. print the sandbox diff against ``main``,
  4. discard the sandbox (default) or merge it into ``main`` (``--apply``).

Prerequisites:
  - a MongoDB 7 replica set on localhost:27017 (see CONTRIBUTING.md)
  - the Argon CLI, serving the API:  argon console --no-browser
  - Python 3.10+ and:  python3 -m pip install argon-agents openai-agents
  - OPENAI_API_KEY in the environment, unless you pass --no-llm

Run:
  python3 examples/openai_agents_sandbox.py --no-llm          # no API key
  python3 examples/openai_agents_sandbox.py                   # real model
  python3 examples/openai_agents_sandbox.py --no-llm --apply  # merge to main

Set ARGON_API_TOKEN if your API requires a bearer token. Each run creates
a new project and leaves it in place so you can inspect it in the console.
"""

from __future__ import annotations

import argparse
import os
import uuid
from typing import Any

from argon_agents import ArgonClient, Sandbox
from pymongo import MongoClient

ACCOUNT_ID = "customer-a"
DISCOUNT_PERCENT = 10
TASK = (
    f"Apply a {DISCOUNT_PERCENT}% discount to account {ACCOUNT_ID}. "
    "Use the apply_discount tool exactly once, then say what you changed."
)


def seed_parent(argon: ArgonClient, project: str) -> None:
    """Create the fixture collection on ``main``, before any sandbox exists.

    The agent only writes to a collection that already exists in the parent.
    """
    with MongoClient(argon.checkout(project, "main", actor="seed")) as client:
        main = client.get_default_database()
        main.create_collection(
            "accounts", changeStreamPreAndPostImages={"enabled": True}
        )
        main.accounts.insert_many(
            [
                {"_id": "customer-a", "spend": 1200, "discount_percent": 0},
                {"_id": "customer-b", "spend": 100, "discount_percent": 0},
            ]
        )


def make_apply_discount(sandbox: Sandbox):
    """Return the agent's only tool, bound to the sandbox database."""
    accounts = sandbox.pymongo_database().accounts

    def apply_discount(account_id: str, percent: int) -> str:
        """Set an account's discount percentage.

        Args:
            account_id: The account's _id, for example "customer-a".
            percent: The discount to set, from 0 to 50.
        """
        if not 0 <= percent <= 50:
            return f"Refused: {percent}% is outside 0-50."
        result = accounts.update_one(
            {"_id": account_id}, {"$set": {"discount_percent": percent}}
        )
        if result.matched_count == 0:
            return f"No account with _id {account_id!r}."
        return f"Set discount_percent={percent} on {account_id}."

    return apply_discount


def run_agent(sandbox: Sandbox, no_llm: bool) -> str:
    """Let the agent do its task in the sandbox and return its final answer."""
    apply_discount = make_apply_discount(sandbox)
    if no_llm:
        # Deterministic stand-in for the model: the tool call it should make.
        return apply_discount(ACCOUNT_ID, DISCOUNT_PERCENT)

    # Imported here so --no-llm runs without the OpenAI Agents SDK installed.
    from agents import Agent, Runner, function_tool

    agent = Agent(
        name="Discount agent",
        instructions="You update customer accounts using the tools you are given.",
        tools=[function_tool(apply_discount)],
    )
    return str(Runner.run_sync(agent, TASK).final_output)


def print_diff(diff: dict[str, Any]) -> None:
    """Print each changed document as field: before -> after."""
    changes = diff.get("changes") or []
    print(
        f"Diff {diff['source_branch']} -> {diff['target_branch']}: "
        f"{len(changes)} change(s)"
    )
    for change in changes:
        before, after = change.get("before") or {}, change.get("document") or {}
        print(f"  {change['collection']}/{change['document_id']}")
        for field in sorted(set(before) | set(after)):
            if before.get(field) != after.get(field):
                print(f"    {field}: {before.get(field)!r} -> {after.get(field)!r}")


def main_discount(argon: ArgonClient, project: str) -> int:
    """Read the account's discount on ``main`` through a fresh sandbox."""
    check = argon.create_sandbox(project, name="verify", actor="verify")
    try:
        account = check.pymongo_database().accounts.find_one({"_id": ACCOUNT_ID})
        return account["discount_percent"]
    finally:
        check.discard()


def run(api_url: str, no_llm: bool, apply: bool) -> None:
    """Seed main, let the agent work in a sandbox, then review the result.

    Args:
        api_url: Base URL of the Argon API, for example http://127.0.0.1:1818.
        no_llm: Replace the model call with a fixed tool call.
        apply: Merge the sandbox into main before discarding it. Without
            this, main is never written after seeding.
    """
    argon = ArgonClient(api_url, token=os.environ.get("ARGON_API_TOKEN"))
    project = "openai-agents-" + uuid.uuid4().hex[:8]
    argon.create_project(project)
    seed_parent(argon, project)
    print(f"Project {project}: seeded accounts on main")

    sandbox = argon.create_sandbox(
        project, name="discount-agent", actor="agent:discount", ttl_minutes=30
    )
    print(f"Sandbox {sandbox.branch} forked from {sandbox.forked_from}")
    try:
        print("Agent:", run_agent(sandbox, no_llm))
        print_diff(sandbox.diff())

        if apply:
            plan = argon.merge_preview(project, sandbox.branch)
            if plan.get("conflicts"):
                raise SystemExit(
                    f"Merge plan {plan['id']} has conflicts; not applying."
                )
            result = argon.merge_apply(plan["id"])
            print(f"Applied merge plan {plan['id']}: {result['applied']} change(s)")
    finally:
        # Runs on success and on failure, so no sandbox waits for its TTL.
        sandbox.discard()
        print(f"Discarded sandbox {sandbox.branch}")
    print(f"main: {ACCOUNT_ID} discount_percent = {main_discount(argon, project)}")


if __name__ == "__main__":
    parser = argparse.ArgumentParser(
        description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter
    )
    parser.add_argument(
        "--api",
        default=os.environ.get("ARGON_API_URL", "http://127.0.0.1:1818"),
        help="Argon API URL (default: %(default)s)",
    )
    parser.add_argument(
        "--no-llm",
        action="store_true",
        help="replace the model call with a fixed tool call (no API key needed)",
    )
    parser.add_argument(
        "--apply",
        action="store_true",
        help="merge the sandbox into main instead of only discarding it",
    )
    args = parser.parse_args()
    run(args.api, args.no_llm, args.apply)
