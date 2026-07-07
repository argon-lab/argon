# Driver-compatibility harness

Real, **unmodified** official drivers run real-world workloads against the
physical database of a checked-out Argon branch; afterwards `compat-verify`
requires the WAL materialization to reproduce the physical state exactly,
document by document.

| Harness | Driver | Exercises |
|---|---|---|
| `pymongo/` | PyMongo (pytest) | CRUD, update operators, upserts, mixed `bulk_write`, secondary/compound indexes, sort/skip/limit, aggregation, `distinct`, multi-document transactions, cursor batching |
| `mongoose/` | Mongoose (Node ODM) | Schema models with validation and defaults, `create`/`insertMany`, `findOneAndUpdate` with operators, lean queries, aggregation, schema indexes, `updateMany`/`deleteMany` |

Run locally (needs a MongoDB replica set on `MONGODB_URI`, Go, Python 3
with `pymongo`+`pytest`, Node):

```bash
bash compat/run.sh
```

## What this proves — and what it doesn't

It proves the drop-in contract Argon actually makes: **applications using
official drivers work unchanged against a checked-out branch, and every
write becomes versioned history that reproduces the database state
exactly.** The query side is real mongod, so query behavior is mongod's
by construction; the part in question — and verified here — is capture
fidelity across the whole write surface, including transactions.

It is *not* the MongoDB vendors' internal driver test suites; those test
driver-server protocol internals against controlled topologies and
failpoints, which is orthogonal to Argon (drivers talk to an ordinary
mongod either way).
