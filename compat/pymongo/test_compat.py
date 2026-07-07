"""Driver-compatibility harness: PyMongo against a checked-out Argon branch.

This is not MongoDB's internal driver test suite — it is a real-world
workload through the unmodified official Python driver, run against the
physical database of a checked-out branch. It exercises CRUD, update
operators, bulk writes, indexes, sort/skip/limit, aggregation, distinct
and multi-document transactions, asserting driver-level results. The
convergence check (compat-verify) afterwards asserts that Argon's WAL
reproduces exactly what the driver did.

The branch connection string arrives via ARGON_BRANCH_URI.
"""

import os

import pytest
from pymongo import ASCENDING, DESCENDING, MongoClient, UpdateOne, InsertOne, DeleteOne


@pytest.fixture(scope="module")
def db():
    uri = os.environ.get("ARGON_BRANCH_URI")
    assert uri, "ARGON_BRANCH_URI must point at a checked-out branch"
    client = MongoClient(uri)
    database = client.get_default_database()
    yield database
    client.close()


def test_crud_roundtrip(db):
    users = db.users
    result = users.insert_one({"_id": "alice", "score": 10, "tags": ["a", "b"]})
    assert result.inserted_id == "alice"

    many = users.insert_many([{"_id": f"user-{i}", "score": i} for i in range(20)])
    assert len(many.inserted_ids) == 20

    updated = users.update_one({"_id": "alice"}, {"$inc": {"score": 5}, "$push": {"tags": "c"}})
    assert updated.modified_count == 1
    doc = users.find_one({"_id": "alice"})
    assert doc["score"] == 15
    assert doc["tags"] == ["a", "b", "c"]

    replaced = users.replace_one({"_id": "user-0"}, {"replaced": True})
    assert replaced.modified_count == 1

    upserted = users.update_one({"_id": "ghost"}, {"$setOnInsert": {"born": True}}, upsert=True)
    assert upserted.upserted_id == "ghost"

    deleted = users.delete_many({"score": {"$gte": 15}})
    assert deleted.deleted_count >= 1


def test_queries_indexes_and_aggregation(db):
    orders = db.orders
    orders.insert_many(
        [{"_id": f"o{i}", "amount": i * 10, "region": ["east", "west"][i % 2]} for i in range(50)]
    )

    orders.create_index([("amount", DESCENDING)])
    orders.create_index([("region", ASCENDING), ("amount", DESCENDING)])

    top = list(orders.find().sort("amount", DESCENDING).skip(2).limit(3))
    assert [d["_id"] for d in top] == ["o47", "o46", "o45"]

    assert orders.count_documents({"region": "east"}) == 25
    assert sorted(orders.distinct("region")) == ["east", "west"]

    pipeline = [
        {"$match": {"amount": {"$gte": 100}}},
        {"$group": {"_id": "$region", "total": {"$sum": "$amount"}}},
        {"$sort": {"_id": 1}},
    ]
    groups = list(orders.aggregate(pipeline))
    assert len(groups) == 2
    assert groups[0]["_id"] == "east"


def test_bulk_write_mixed(db):
    items = db.items
    result = items.bulk_write(
        [
            InsertOne({"_id": "i1", "n": 1}),
            InsertOne({"_id": "i2", "n": 2}),
            UpdateOne({"_id": "i1"}, {"$set": {"n": 100}}),
            DeleteOne({"_id": "i2"}),
            UpdateOne({"_id": "i3"}, {"$set": {"n": 3}}, upsert=True),
        ],
        ordered=True,
    )
    assert result.inserted_count == 2
    assert result.modified_count == 1
    assert result.deleted_count == 1
    assert result.upserted_count == 1
    assert items.find_one({"_id": "i1"})["n"] == 100


def test_multi_document_transaction(db):
    accounts = db.accounts
    ledger = db.ledger
    accounts.insert_one({"_id": "a", "balance": 100})
    accounts.insert_one({"_id": "b", "balance": 0})
    # Creating a collection inside a transaction races the catalog on any
    # MongoDB (TransientTransactionError: WriteConflict) — make sure the
    # ledger collection exists before the transaction starts.
    ledger.insert_one({"_id": "warmup"})
    ledger.delete_one({"_id": "warmup"})

    def txn(session):
        accounts.update_one({"_id": "a"}, {"$inc": {"balance": -40}}, session=session)
        accounts.update_one({"_id": "b"}, {"$inc": {"balance": 40}}, session=session)
        ledger.insert_one({"_id": "t1", "amount": 40}, session=session)

    client = db.client
    with client.start_session() as session:
        # with_transaction retries transient errors, as production code does
        session.with_transaction(txn)

    assert accounts.find_one({"_id": "a"})["balance"] == 60
    assert accounts.find_one({"_id": "b"})["balance"] == 40
    assert ledger.count_documents({}) == 1


def test_cursor_batching(db):
    big = db.big
    big.insert_many([{"_id": i, "payload": "x" * 512} for i in range(500)])
    seen = sum(1 for _ in big.find({}, batch_size=50))
    assert seen == 500
