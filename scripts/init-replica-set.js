// Initialize MongoDB replica set for change streams
rs.initiate({
  _id: "rs0",
  members: [
    {
      _id: 0,
      host: "mongodb:27017"
    }
  ]
});

// Wait for replica set to be ready
while (!rs.isMaster().ismaster) {
  sleep(100);
}

// Create initial database and collections
use argon;

// Create projects collection
db.createCollection("projects");

// Create branches collection with indexes
db.createCollection("branches");
db.branches.createIndex({ "project_id": 1, "name": 1 }, { unique: true });
db.branches.createIndex({ "project_id": 1, "created_at": -1 });
db.branches.createIndex({ "parent_branch": 1 });

// Create change_events collection for change streams tracking
db.createCollection("change_events");
db.change_events.createIndex({ "project_id": 1, "branch_id": 1, "timestamp": -1 });
db.change_events.createIndex({ "resume_token": 1 }, { unique: true });

// Create users collection
db.createCollection("users");
db.users.createIndex({ "email": 1 }, { unique: true });

print("MongoDB replica set and initial collections created successfully!");