#!/usr/bin/env python3
"""Two deterministic agent proposals over one pinned business dataset.

Run `argon console --no-browser`, then:
  python3 -m pip install pymongo
  python3 examples/pinned_agents.py --api http://127.0.0.1:1818

This exercises real MongoDB writes and REST control operations, without
requiring an LLM account. The accepted project and pin remain for inspection.
"""

import argparse
import json
import os
import urllib.error
import urllib.request
import uuid

from pymongo import MongoClient


def run(base_url):
    def api(method, path, body=None):
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
            with urllib.request.urlopen(request, timeout=40) as response:
                return json.load(response)
        except urllib.error.HTTPError as error:
            raise RuntimeError(f"{method} {path}: {error.read().decode()}") from error

    project = "discount-review-" + uuid.uuid4().hex[:10]
    api("POST", "/projects", {"name": project})
    prefix = "/projects/" + project
    baseline = api("POST", prefix + "/branches/main/checkout", {"actor": "seed"})
    with MongoClient(baseline["connection_string"]) as main_client:
        main = main_client.get_default_database()
        main.create_collection("accounts", changeStreamPreAndPostImages={"enabled": True})
        main.accounts.insert_many([
            {"_id": "customer-a", "spend": 1200, "discount_percent": 0},
            {"_id": "customer-b", "spend": 100, "discount_percent": 0},
        ])
        pin = api("POST", prefix + "/pins", {"name": "baseline-v1", "note": "Identical business data for both proposals"})
        sandboxes = []
        for name, discount in (("agent-a", 10), ("agent-b", 50)):
            sandbox = api("POST", prefix + "/pins/baseline-v1/sandboxes", {
                "name": name, "actor": name, "ttl_minutes": 60,
            })
            with MongoClient(sandbox["connection_string"]) as client:
                accounts = client.get_default_database().accounts
                assert accounts.find_one({"_id": "customer-a"})["discount_percent"] == 0
                accounts.update_one({"_id": "customer-a"}, {"$set": {"discount_percent": discount}})
            sandboxes.append(name)

        # Both proposals are isolated; nothing has changed on main yet.
        assert main.accounts.find_one({"_id": "customer-a"})["discount_percent"] == 0
        reviews = {name: api("GET", prefix + f"/branches/{name}/diff") for name in sandboxes}
        assert all(len(review["changes"]) == 1 for review in reviews.values())
        # A simple business rule accepts the 10% proposal and rejects the 50% one.
        approved = api("POST", prefix + "/branches/agent-a/merge-preview", {})
        assert not approved["conflicts"]
        result = api("POST", "/merge-plans/" + approved["id"] + "/apply", {})
        assert result["applied"] == 1
        assert main.accounts.find_one({"_id": "customer-a"})["discount_percent"] == 10
        assert main.accounts.find_one({"_id": "customer-b"})["discount_percent"] == 0
        api("DELETE", prefix + "/sandboxes/agent-b")
        api("DELETE", prefix + "/sandboxes/agent-a")

    print(json.dumps({"project": project, "pin": pin["name"], "pin_lsn": pin["lsn"],
                      "accepted": "agent-a", "discarded": "agent-b", "discount_percent": 10}, indent=2))


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--api", default="http://127.0.0.1:1818")
    run(parser.parse_args().api)
