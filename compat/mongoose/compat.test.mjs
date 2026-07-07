// Driver-compatibility harness: Mongoose against a checked-out Argon branch.
//
// A real-world ODM workload through unmodified Mongoose: schema models,
// validation, saves, queries, findOneAndUpdate, aggregation, indexes and
// deletes. The convergence check (compat-verify) afterwards asserts that
// Argon's WAL reproduces exactly what the ODM did.
//
// The branch connection string arrives via ARGON_BRANCH_URI.

import assert from "node:assert/strict";
import mongoose from "mongoose";

const uri = process.env.ARGON_BRANCH_URI;
assert.ok(uri, "ARGON_BRANCH_URI must point at a checked-out branch");

await mongoose.connect(uri);

const productSchema = new mongoose.Schema(
  {
    _id: String,
    name: { type: String, required: true },
    price: { type: Number, min: 0 },
    stock: { type: Number, default: 0 },
    tags: [String],
  },
  { versionKey: false }
);
productSchema.index({ price: -1 });
const Product = mongoose.model("Product", productSchema, "products");

// Create through the model, including defaults and validation.
await Product.create({ _id: "laptop", name: "Laptop", price: 1000, stock: 10, tags: ["tech"] });
await Product.insertMany(
  Array.from({ length: 30 }, (_, i) => ({
    _id: `gadget-${i}`,
    name: `Gadget ${i}`,
    price: i * 10,
    stock: i,
  }))
);

// Validation actually fires.
await assert.rejects(Product.create({ _id: "bad", price: 5 }), /name.*required/i);

// findOneAndUpdate with operators, returning the new document.
const discounted = await Product.findOneAndUpdate(
  { _id: "laptop" },
  { $inc: { price: -100 }, $push: { tags: "sale" } },
  { new: true }
);
assert.equal(discounted.price, 900);
assert.deepEqual(discounted.tags, ["tech", "sale"]);

// Queries with sort/limit/lean.
const expensive = await Product.find({ price: { $gte: 200 } }).sort({ price: -1 }).limit(3).lean();
assert.equal(expensive.length, 3);
assert.equal(expensive[0]._id, "laptop");

// Aggregation through the ODM.
const stats = await Product.aggregate([
  { $group: { _id: null, avgPrice: { $avg: "$price" }, count: { $sum: 1 } } },
]);
assert.equal(stats[0].count, 31);

// Index creation through the schema.
await Product.createIndexes();
const indexes = await Product.collection.indexes();
assert.ok(indexes.some((ix) => ix.key.price === -1));

// Updates and deletes en masse.
const restocked = await Product.updateMany({ stock: { $lt: 5 } }, { $set: { stock: 5 } });
assert.ok(restocked.modifiedCount >= 4);
const removed = await Product.deleteMany({ price: { $lt: 50 } });
assert.ok(removed.deletedCount >= 4);

await mongoose.disconnect();
console.log("mongoose compat workload passed");
