package wal

import (
	"context"

	branchwal "github.com/argon-lab/argon/internal/branch/wal"
	"github.com/argon-lab/argon/internal/wal"
	"go.mongodb.org/mongo-driver/mongo"
)

// Database represents a WAL-enabled database
type Database struct {
	name         string
	branch       *wal.Branch
	wal          *wal.Service
	branches     *branchwal.BranchService
	materializer Materializer
	underlying   *mongo.Database
}

// NewDatabase creates a new WAL-enabled database
func NewDatabase(
	name string,
	branch *wal.Branch,
	walService *wal.Service,
	branchService *branchwal.BranchService,
	materializer Materializer,
	underlying *mongo.Database,
) *Database {
	return &Database{
		name:         name,
		branch:       branch,
		wal:          walService,
		branches:     branchService,
		materializer: materializer,
		underlying:   underlying,
	}
}

// Collection returns a WAL-enabled collection
func (d *Database) Collection(name string) *Collection {
	return NewCollection(
		name,
		d.branch,
		d.wal,
		d.branches,
		d.materializer,
		d.underlying.Collection(name),
	)
}

// Name returns the database name
func (d *Database) Name() string {
	return d.name
}

// Branch returns the current branch
func (d *Database) Branch() *wal.Branch {
	return d.branch
}

// Drop drops the database (not implemented for WAL)
func (d *Database) Drop(ctx context.Context) error {
	// For WAL, we don't actually drop anything
	// This would be handled at the project level
	return nil
}
