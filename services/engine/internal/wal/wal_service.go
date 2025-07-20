package wal

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// WALEntry represents a single entry in the Write-Ahead Log
type WALEntry struct {
	ID          primitive.ObjectID     `bson:"_id,omitempty"`
	LSN         int64                  `bson:"lsn"`
	Timestamp   time.Time              `bson:"timestamp"`
	Operation   string                 `bson:"operation"` // insert, update, delete, command
	Database    string                 `bson:"database"`
	Collection  string                 `bson:"collection"`
	DocumentID  interface{}            `bson:"document_id,omitempty"`
	Changes     Changes                `bson:"changes"`
	Metadata    Metadata               `bson:"metadata"`
	Checksum    string                 `bson:"checksum"`
}

// Changes represents the before/after state of a document
type Changes struct {
	Before  interface{}            `bson:"before,omitempty"`
	After   interface{}            `bson:"after,omitempty"`
	Delta   map[string]interface{} `bson:"delta,omitempty"`
	Command interface{}            `bson:"command,omitempty"`
}

// Metadata contains additional information about the WAL entry
type Metadata struct {
	UserID    string `bson:"user_id"`
	BranchID  string `bson:"branch_id"`
	ProjectID string `bson:"project_id"`
	TxnID     string `bson:"txn_id,omitempty"`
	Size      int64  `bson:"size"`
}

// WALService manages the Write-Ahead Log
type WALService struct {
	client       *mongo.Client
	db           *mongo.Database
	collection   *mongo.Collection
	currentLSN   int64
	buffer       []WALEntry
	bufferMu     sync.Mutex
	flushTicker  *time.Ticker
	pageSize     int
	compression  bool
	
	// Replication channels
	replicationChan chan WALEntry
	subscribers     map[string]chan WALEntry
	subscribersMu   sync.RWMutex
}

// NewWALService creates a new WAL service
func NewWALService(client *mongo.Client, dbName string, config *Config) (*WALService, error) {
	db := client.Database(dbName)
	collection := db.Collection("wal_entries")
	
	// Create indexes for efficient queries
	indexes := []mongo.IndexModel{
		{
			Keys: bson.D{
				{Key: "lsn", Value: 1},
			},
			Options: options.Index().SetUnique(true),
		},
		{
			Keys: bson.D{
				{Key: "metadata.branch_id", Value: 1},
				{Key: "lsn", Value: 1},
			},
		},
		{
			Keys: bson.D{
				{Key: "timestamp", Value: -1},
			},
		},
		{
			Keys: bson.D{
				{Key: "collection", Value: 1},
				{Key: "lsn", Value: 1},
			},
		},
	}
	
	if _, err := collection.Indexes().CreateMany(context.Background(), indexes); err != nil {
		return nil, fmt.Errorf("failed to create WAL indexes: %w", err)
	}
	
	w := &WALService{
		client:          client,
		db:              db,
		collection:      collection,
		pageSize:        config.PageSize,
		compression:     config.Compression,
		buffer:          make([]WALEntry, 0, config.PageSize),
		replicationChan: make(chan WALEntry, 1000),
		subscribers:     make(map[string]chan WALEntry),
	}
	
	// Initialize LSN from last entry
	if err := w.initializeLSN(context.Background()); err != nil {
		return nil, err
	}
	
	// Start background workers
	w.startFlushWorker(config.FlushInterval)
	w.startReplicationWorker()
	
	return w, nil
}

// initializeLSN sets the current LSN based on the last entry in the WAL
func (w *WALService) initializeLSN(ctx context.Context) error {
	var lastEntry WALEntry
	err := w.collection.FindOne(ctx, bson.M{}, options.FindOne().SetSort(bson.D{{Key: "lsn", Value: -1}})).Decode(&lastEntry)
	
	if err != nil {
		if err == mongo.ErrNoDocuments {
			w.currentLSN = 0
			return nil
		}
		return fmt.Errorf("failed to get last WAL entry: %w", err)
	}
	
	w.currentLSN = lastEntry.LSN
	return nil
}

// Append adds a new entry to the WAL
func (w *WALService) Append(ctx context.Context, entry WALEntry) (int64, error) {
	// Assign LSN atomically
	lsn := atomic.AddInt64(&w.currentLSN, 1)
	entry.LSN = lsn
	entry.Timestamp = time.Now()
	
	// Calculate checksum
	entry.Checksum = w.calculateChecksum(&entry)
	
	// Add to buffer
	w.bufferMu.Lock()
	w.buffer = append(w.buffer, entry)
	shouldFlush := len(w.buffer) >= w.pageSize
	w.bufferMu.Unlock()
	
	// Flush if buffer is full
	if shouldFlush {
		if err := w.Flush(ctx); err != nil {
			return 0, err
		}
	}
	
	// Send to replication channel (non-blocking)
	select {
	case w.replicationChan <- entry:
	default:
		// Channel full, log warning but don't block
	}
	
	return lsn, nil
}

// Flush writes buffered entries to storage
func (w *WALService) Flush(ctx context.Context) error {
	w.bufferMu.Lock()
	if len(w.buffer) == 0 {
		w.bufferMu.Unlock()
		return nil
	}
	
	entries := make([]interface{}, len(w.buffer))
	for i, entry := range w.buffer {
		entries[i] = entry
	}
	w.buffer = w.buffer[:0] // Clear buffer
	w.bufferMu.Unlock()
	
	// Bulk insert
	_, err := w.collection.InsertMany(ctx, entries)
	if err != nil {
		// On error, add entries back to buffer
		w.bufferMu.Lock()
		w.buffer = append(w.buffer, entries...)
		w.bufferMu.Unlock()
		return fmt.Errorf("failed to flush WAL entries: %w", err)
	}
	
	return nil
}

// GetEntriesRange returns WAL entries in the specified LSN range
func (w *WALService) GetEntriesRange(ctx context.Context, startLSN, endLSN int64) ([]WALEntry, error) {
	filter := bson.M{
		"lsn": bson.M{
			"$gte": startLSN,
			"$lte": endLSN,
		},
	}
	
	cursor, err := w.collection.Find(ctx, filter, options.Find().SetSort(bson.D{{Key: "lsn", Value: 1}}))
	if err != nil {
		return nil, err
	}
	defer cursor.Close(ctx)
	
	var entries []WALEntry
	if err := cursor.All(ctx, &entries); err != nil {
		return nil, err
	}
	
	return entries, nil
}

// GetBranchEntries returns all entries for a specific branch
func (w *WALService) GetBranchEntries(ctx context.Context, branchID string, afterLSN int64) ([]WALEntry, error) {
	filter := bson.M{
		"metadata.branch_id": branchID,
	}
	
	if afterLSN > 0 {
		filter["lsn"] = bson.M{"$gt": afterLSN}
	}
	
	cursor, err := w.collection.Find(ctx, filter, options.Find().SetSort(bson.D{{Key: "lsn", Value: 1}}))
	if err != nil {
		return nil, err
	}
	defer cursor.Close(ctx)
	
	var entries []WALEntry
	if err := cursor.All(ctx, &entries); err != nil {
		return nil, err
	}
	
	return entries, nil
}

// FindLSNAtTime returns the LSN of the last entry before the specified time
func (w *WALService) FindLSNAtTime(ctx context.Context, targetTime time.Time) (int64, error) {
	var entry WALEntry
	err := w.collection.FindOne(
		ctx,
		bson.M{"timestamp": bson.M{"$lte": targetTime}},
		options.FindOne().SetSort(bson.D{{Key: "timestamp", Value: -1}}),
	).Decode(&entry)
	
	if err != nil {
		if err == mongo.ErrNoDocuments {
			return 0, nil
		}
		return 0, err
	}
	
	return entry.LSN, nil
}

// Subscribe creates a subscription for real-time WAL updates
func (w *WALService) Subscribe(id string) <-chan WALEntry {
	w.subscribersMu.Lock()
	defer w.subscribersMu.Unlock()
	
	ch := make(chan WALEntry, 100)
	w.subscribers[id] = ch
	return ch
}

// Unsubscribe removes a subscription
func (w *WALService) Unsubscribe(id string) {
	w.subscribersMu.Lock()
	defer w.subscribersMu.Unlock()
	
	if ch, ok := w.subscribers[id]; ok {
		close(ch)
		delete(w.subscribers, id)
	}
}

// startFlushWorker starts a background worker that periodically flushes the buffer
func (w *WALService) startFlushWorker(interval time.Duration) {
	w.flushTicker = time.NewTicker(interval)
	
	go func() {
		for range w.flushTicker.C {
			if err := w.Flush(context.Background()); err != nil {
				// Log error but don't stop the worker
				fmt.Printf("WAL flush error: %v\n", err)
			}
		}
	}()
}

// startReplicationWorker distributes WAL entries to subscribers
func (w *WALService) startReplicationWorker() {
	go func() {
		for entry := range w.replicationChan {
			w.subscribersMu.RLock()
			for _, ch := range w.subscribers {
				select {
				case ch <- entry:
				default:
					// Subscriber channel full, skip
				}
			}
			w.subscribersMu.RUnlock()
		}
	}()
}

// calculateChecksum generates a checksum for a WAL entry
func (w *WALService) calculateChecksum(entry *WALEntry) string {
	// Simple checksum - in production use proper hashing
	data, _ := json.Marshal(entry.Changes)
	return fmt.Sprintf("%x", len(data))
}

// Close gracefully shuts down the WAL service
func (w *WALService) Close(ctx context.Context) error {
	// Stop flush worker
	if w.flushTicker != nil {
		w.flushTicker.Stop()
	}
	
	// Final flush
	if err := w.Flush(ctx); err != nil {
		return err
	}
	
	// Close replication channel
	close(w.replicationChan)
	
	// Close all subscriber channels
	w.subscribersMu.Lock()
	for _, ch := range w.subscribers {
		close(ch)
	}
	w.subscribers = make(map[string]chan WALEntry)
	w.subscribersMu.Unlock()
	
	return nil
}

// Config holds WAL configuration
type Config struct {
	PageSize      int           // Number of entries per page
	FlushInterval time.Duration // How often to flush buffer
	Compression   bool          // Enable compression
	Retention     time.Duration // How long to keep WAL entries
}