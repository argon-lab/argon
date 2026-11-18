"""Agent-written code. host.py uploads this file and runs it inside the boxd machine.

It receives the restricted connection string in ARGON_SANDBOX_URI and only
ever sees the Argon sandbox's database.
"""

import json
import os

from pymongo import MongoClient
from pymongo.errors import OperationFailure

UNAUTHORIZED = 13


def main() -> None:
    """Triage the open tickets in the sandbox and leave a note for the reviewer."""
    with MongoClient(
        os.environ["ARGON_SANDBOX_URI"], serverSelectionTimeoutMS=10000
    ) as client:
        db = client.get_default_database()

        # The restricted user can read and write the sandbox, and nothing else.
        try:
            client[os.environ["ARGON_MAIN_DB"]].tickets.find_one()
        except OperationFailure as error:
            if error.code != UNAUTHORIZED:
                raise
        else:
            raise SystemExit("the sandbox user can read the main branch")

        closed = db.tickets.update_many(
            {"status": "open", "resolution": {"$exists": True}},
            {"$set": {"status": "closed"}},
        )
        escalated = db.tickets.update_many(
            {"status": "open", "priority": "high"},
            {"$set": {"status": "escalated"}},
        )
        # A collection the agent creates itself. Argon 2.3.1 and newer capture
        # it exactly without `argon collections prepare`.
        db.triage_notes.insert_one(
            {
                "_id": "triage-1",
                "closed": closed.modified_count,
                "escalated": escalated.modified_count,
            }
        )

    print(
        json.dumps(
            {
                "database": db.name,
                "closed": closed.modified_count,
                "escalated": escalated.modified_count,
            }
        )
    )


if __name__ == "__main__":
    main()
