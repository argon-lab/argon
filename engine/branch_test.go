package engine

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
)

// MockStorage implements Storage interface for testing
type MockStorage struct {
	data map[string][]byte
}

func NewMockStorage() *MockStorage {
	return &MockStorage{
		data: make(map[string][]byte),
	}
}

func (m *MockStorage) Store(ctx context.Context, key string, data []byte) error {
	m.data[key] = data
	return nil
}

func (m *MockStorage) Retrieve(ctx context.Context, key string) ([]byte, error) {
	data, ok := m.data[key]
	if !ok {
		return nil, mongo.ErrNoDocuments
	}
	return data, nil
}

func (m *MockStorage) Delete(ctx context.Context, key string) error {
	delete(m.data, key)
	return nil
}

func (m *MockStorage) Exists(ctx context.Context, key string) bool {
	_, ok := m.data[key]
	return ok
}

func TestNewBranchEngine(t *testing.T) {
	storage := NewMockStorage()
	engine := NewBranchEngine(nil, storage)
	
	assert.NotNil(t, engine)
	assert.NotNil(t, engine.storage)
}

func TestCreateBranch(t *testing.T) {
	tests := []struct {
		name        string
		branchName  string
		parentName  string
		expectError bool
	}{
		{
			name:        "Valid branch creation",
			branchName:  "feature-test",
			parentName:  "main",
			expectError: false,
		},
		{
			name:        "Invalid branch name",
			branchName:  "invalid/branch/name",
			parentName:  "main",
			expectError: true,
		},
		{
			name:        "Empty branch name",
			branchName:  "",
			parentName:  "main",
			expectError: true,
		},
		{
			name:        "Reserved branch name",
			branchName:  "HEAD",
			parentName:  "main",
			expectError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			storage := NewMockStorage()
			engine := NewBranchEngine(nil, storage)

			branch, err := engine.CreateBranch(context.Background(), tt.branchName, tt.parentName)

			if tt.expectError {
				assert.Error(t, err)
				assert.Nil(t, branch)
			} else {
				assert.NoError(t, err)
				assert.NotNil(t, branch)
				assert.Equal(t, tt.branchName, branch.Name)
				assert.Equal(t, tt.parentName, branch.Parent)
				assert.Equal(t, BranchStatusActive, branch.Status)
			}
		})
	}
}

func TestDeleteBranch(t *testing.T) {
	storage := NewMockStorage()
	engine := NewBranchEngine(nil, storage)

	// Create a branch first
	branch, err := engine.CreateBranch(context.Background(), "test-branch", "main")
	require.NoError(t, err)

	// Test deleting existing branch
	err = engine.DeleteBranch(context.Background(), branch.Name)
	assert.NoError(t, err)

	// Test deleting non-existent branch
	err = engine.DeleteBranch(context.Background(), "non-existent")
	assert.Error(t, err)

	// Test deleting main branch (should fail)
	err = engine.DeleteBranch(context.Background(), "main")
	assert.Error(t, err)
}

func TestListBranches(t *testing.T) {
	storage := NewMockStorage()
	engine := NewBranchEngine(nil, storage)

	// Create multiple branches
	branches := []string{"feature-1", "feature-2", "bugfix-1"}
	for _, name := range branches {
		_, err := engine.CreateBranch(context.Background(), name, "main")
		require.NoError(t, err)
	}

	// List all branches
	result, err := engine.ListBranches(context.Background())
	assert.NoError(t, err)
	assert.Len(t, result, len(branches)+1) // +1 for main branch
}

func TestGetBranch(t *testing.T) {
	storage := NewMockStorage()
	engine := NewBranchEngine(nil, storage)

	// Create a branch
	created, err := engine.CreateBranch(context.Background(), "test-branch", "main")
	require.NoError(t, err)

	// Get existing branch
	branch, err := engine.GetBranch(context.Background(), "test-branch")
	assert.NoError(t, err)
	assert.Equal(t, created.ID, branch.ID)
	assert.Equal(t, created.Name, branch.Name)

	// Get non-existent branch
	branch, err = engine.GetBranch(context.Background(), "non-existent")
	assert.Error(t, err)
	assert.Nil(t, branch)
}

func TestBranchValidation(t *testing.T) {
	tests := []struct {
		name       string
		branchName string
		valid      bool
	}{
		{"Valid name", "feature-123", true},
		{"Valid with underscore", "feature_test", true},
		{"Valid with numbers", "branch123", true},
		{"Invalid with slash", "feature/test", false},
		{"Invalid with space", "feature test", false},
		{"Invalid with special char", "feature@test", false},
		{"Too short", "a", false},
		{"Too long", "this-is-a-very-long-branch-name-that-exceeds-the-maximum-allowed-length-for-branch-names", false},
		{"Reserved name main", "main", false},
		{"Reserved name master", "master", false},
		{"Reserved name HEAD", "HEAD", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateBranchName(tt.branchName)
			if tt.valid {
				assert.NoError(t, err)
			} else {
				assert.Error(t, err)
			}
		})
	}
}

func TestBranchMetadata(t *testing.T) {
	storage := NewMockStorage()
	engine := NewBranchEngine(nil, storage)

	// Create branch with metadata
	branch, err := engine.CreateBranchWithMetadata(context.Background(), "test-branch", "main", map[string]interface{}{
		"description": "Test branch for feature X",
		"owner":       "test@example.com",
		"jira_ticket": "PROJ-123",
	})

	assert.NoError(t, err)
	assert.NotNil(t, branch.Metadata)
	assert.Equal(t, "Test branch for feature X", branch.Metadata["description"])
	assert.Equal(t, "test@example.com", branch.Metadata["owner"])
	assert.Equal(t, "PROJ-123", branch.Metadata["jira_ticket"])
}

func TestBranchStats(t *testing.T) {
	storage := NewMockStorage()
	engine := NewBranchEngine(nil, storage)

	branch, err := engine.CreateBranch(context.Background(), "test-branch", "main")
	require.NoError(t, err)

	// Update branch stats
	stats := &BranchStats{
		Documents:   1000,
		StorageSize: 1048576, // 1MB
		Collections: []string{"users", "orders"},
		LastWrite:   time.Now(),
	}

	err = engine.UpdateBranchStats(context.Background(), branch.Name, stats)
	assert.NoError(t, err)

	// Get branch with updated stats
	updated, err := engine.GetBranch(context.Background(), branch.Name)
	assert.NoError(t, err)
	assert.Equal(t, stats.Documents, updated.Stats.Documents)
	assert.Equal(t, stats.StorageSize, updated.Stats.StorageSize)
	assert.Equal(t, stats.Collections, updated.Stats.Collections)
}

func TestConcurrentBranchOperations(t *testing.T) {
	storage := NewMockStorage()
	engine := NewBranchEngine(nil, storage)

	// Test concurrent branch creation
	done := make(chan bool, 10)
	errors := make(chan error, 10)

	for i := 0; i < 10; i++ {
		go func(idx int) {
			_, err := engine.CreateBranch(context.Background(), fmt.Sprintf("concurrent-%d", idx), "main")
			if err != nil {
				errors <- err
			}
			done <- true
		}(i)
	}

	// Wait for all goroutines
	for i := 0; i < 10; i++ {
		<-done
	}

	close(errors)
	
	// Check for errors
	for err := range errors {
		t.Errorf("Concurrent operation failed: %v", err)
	}

	// Verify all branches were created
	branches, err := engine.ListBranches(context.Background())
	assert.NoError(t, err)
	assert.GreaterOrEqual(t, len(branches), 10)
}

func BenchmarkBranchCreation(b *testing.B) {
	storage := NewMockStorage()
	engine := NewBranchEngine(nil, storage)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		engine.CreateBranch(context.Background(), fmt.Sprintf("bench-%d", i), "main")
	}
}

func BenchmarkBranchLookup(b *testing.B) {
	storage := NewMockStorage()
	engine := NewBranchEngine(nil, storage)

	// Create test branches
	for i := 0; i < 1000; i++ {
		engine.CreateBranch(context.Background(), fmt.Sprintf("branch-%d", i), "main")
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		engine.GetBranch(context.Background(), fmt.Sprintf("branch-%d", i%1000))
	}
}