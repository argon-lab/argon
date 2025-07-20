package materializer

import (
	"fmt"
	"strings"

	"github.com/argon-lab/argon/internal/wal"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

// Service materializes current state from WAL entries
type Service struct {
	wal *wal.Service
}

// NewService creates a new materializer service
func NewService(walService *wal.Service) *Service {
	return &Service{
		wal: walService,
	}
}

// MaterializeCollection builds the current state of a collection for a branch
func (s *Service) MaterializeCollection(branch *wal.Branch, collection string) (map[string]bson.M, error) {
	// For branches created from a historical point (BaseLSN > 0), we need to include
	// entries from before the branch was created up to the BaseLSN
	entries := []*wal.Entry{}
	var err error
	
	if branch.BaseLSN > 0 {
		// This is a branch created from a historical point
		// Get entries up to the base LSN from the global WAL
		baseEntries, err := s.wal.GetProjectEntries(branch.ProjectID, collection, 0, branch.BaseLSN)
		if err != nil {
			return nil, fmt.Errorf("failed to get base entries: %w", err)
		}
		entries = append(entries, baseEntries...)
	}
	
	// Get entries specific to this branch (after it was created)
	branchEntries, err := s.wal.GetBranchEntries(branch.ID, collection, branch.BaseLSN+1, branch.HeadLSN)
	if err != nil {
		return nil, fmt.Errorf("failed to get branch entries: %w", err)
	}
	entries = append(entries, branchEntries...)

	// Build state by replaying entries
	state := make(map[string]bson.M)
	
	for _, entry := range entries {
		if err := s.ApplyEntry(state, entry); err != nil {
			return nil, fmt.Errorf("failed to apply entry LSN %d: %w", entry.LSN, err)
		}
	}

	return state, nil
}

// MaterializeBranch builds the complete state of all collections in a branch
func (s *Service) MaterializeBranch(branch *wal.Branch) (map[string]map[string]bson.M, error) {
	// For branches created from a historical point, include base entries
	entries := []*wal.Entry{}
	var err error
	
	if branch.BaseLSN > 0 {
		// Get entries up to the base LSN from the global WAL
		baseEntries, err := s.wal.GetProjectEntries(branch.ProjectID, "", 0, branch.BaseLSN)
		if err != nil {
			return nil, fmt.Errorf("failed to get base entries: %w", err)
		}
		entries = append(entries, baseEntries...)
	}
	
	// Get entries specific to this branch
	branchEntries, err := s.wal.GetBranchEntries(branch.ID, "", branch.BaseLSN+1, branch.HeadLSN)
	if err != nil {
		return nil, fmt.Errorf("failed to get branch entries: %w", err)
	}
	entries = append(entries, branchEntries...)

	// Build state by collection
	state := make(map[string]map[string]bson.M)
	
	for _, entry := range entries {
		if entry.Collection == "" {
			continue // Skip non-collection operations
		}
		
		// Initialize collection state if needed
		if _, exists := state[entry.Collection]; !exists {
			state[entry.Collection] = make(map[string]bson.M)
		}
		
		if err := s.ApplyEntry(state[entry.Collection], entry); err != nil {
			return nil, fmt.Errorf("failed to apply entry LSN %d: %w", entry.LSN, err)
		}
	}

	return state, nil
}

// MaterializeDocument gets the current state of a specific document
func (s *Service) MaterializeDocument(branch *wal.Branch, collection, documentID string) (bson.M, error) {
	// Get all entries for the document
	entries, err := s.wal.GetDocumentHistory(branch.ID, collection, documentID, 0, branch.HeadLSN)
	if err != nil {
		return nil, fmt.Errorf("failed to get document history: %w", err)
	}

	// Build document state by replaying entries
	state := make(map[string]bson.M)
	
	for _, entry := range entries {
		if err := s.ApplyEntry(state, entry); err != nil {
			return nil, fmt.Errorf("failed to apply entry LSN %d: %w", entry.LSN, err)
		}
	}

	// Return the document if it exists
	if doc, exists := state[documentID]; exists {
		return doc, nil
	}

	return nil, nil // Document doesn't exist or was deleted
}

// ApplyEntry applies a WAL entry to the current state
// Exported for use by time travel service
func (s *Service) ApplyEntry(state map[string]bson.M, entry *wal.Entry) error {
	switch entry.Operation {
	case wal.OpInsert:
		return s.applyInsert(state, entry)
	case wal.OpUpdate:
		return s.applyUpdate(state, entry)
	case wal.OpDelete:
		return s.applyDelete(state, entry)
	default:
		// Skip unknown operations
		return nil
	}
}

// applyInsert applies an insert operation
func (s *Service) applyInsert(state map[string]bson.M, entry *wal.Entry) error {
	// Unmarshal the document
	var doc bson.M
	if err := bson.Unmarshal(entry.Document, &doc); err != nil {
		return fmt.Errorf("failed to unmarshal document: %w", err)
	}

	// Extract document ID
	docID := entry.DocumentID
	if docID == "" {
		// Try to extract from document
		if id, exists := doc["_id"]; exists {
			docID = convertIDToString(id)
		}
	}

	if docID == "" {
		return fmt.Errorf("document has no ID")
	}

	// Store in state
	state[docID] = doc
	return nil
}

// applyUpdate applies an update operation
func (s *Service) applyUpdate(state map[string]bson.M, entry *wal.Entry) error {
	// For MVP, updates store filter and update operation
	// Parse the combined document
	var updateDoc bson.M
	if err := bson.Unmarshal(entry.Document, &updateDoc); err != nil {
		return fmt.Errorf("failed to unmarshal update document: %w", err)
	}

	// Extract filter and update - they can be either bson.M or bson.Raw
	var filter, update bson.M
	
	// Handle filter
	switch f := updateDoc["filter"].(type) {
	case bson.M:
		filter = f
	case map[string]interface{}:
		filter = bson.M(f)
	case bson.Raw:
		if err := bson.Unmarshal(f, &filter); err != nil {
			return fmt.Errorf("failed to unmarshal filter from Raw: %w", err)
		}
	default:
		return fmt.Errorf("invalid filter format: %T", f)
	}
	
	// Handle update
	switch u := updateDoc["update"].(type) {
	case bson.M:
		update = u
	case map[string]interface{}:
		update = bson.M(u)
	case bson.Raw:
		if err := bson.Unmarshal(u, &update); err != nil {
			return fmt.Errorf("failed to unmarshal update from Raw: %w", err)
		}
	default:
		return fmt.Errorf("invalid update format: %T", u)
	}

	// Find matching documents and apply update
	for docID, doc := range state {
		if matchesFilter(doc, filter) {
			// Apply update operations
			if err := applyUpdateOperations(doc, update); err != nil {
				return fmt.Errorf("failed to apply update to document %s: %w", docID, err)
			}
			// Only update first match for UpdateOne
			break
		}
	}

	return nil
}

// applyDelete applies a delete operation
func (s *Service) applyDelete(state map[string]bson.M, entry *wal.Entry) error {
	// For MVP, deletes store the filter
	var filter bson.M
	if err := bson.Unmarshal(entry.Document, &filter); err != nil {
		return fmt.Errorf("failed to unmarshal filter: %w", err)
	}

	// Find and delete matching documents
	for docID, doc := range state {
		if matchesFilter(doc, filter) {
			delete(state, docID)
			// Only delete first match for DeleteOne
			break
		}
	}

	return nil
}

// convertIDToString converts various ID types to string
func convertIDToString(id interface{}) string {
	switch v := id.(type) {
	case primitive.ObjectID:
		return v.Hex()
	case string:
		return v
	default:
		return fmt.Sprintf("%v", id)
	}
}

// matchesFilter checks if a document matches a filter
func matchesFilter(doc, filter bson.M) bool {
	// Empty filter matches all
	if len(filter) == 0 {
		return true
	}

	// Check each filter field
	for key, expected := range filter {
		actual, exists := doc[key]
		if !exists {
			return false
		}

		// Handle different operator types
		switch exp := expected.(type) {
		case bson.M:
			// Handle operators like $gt, $lt, etc.
			if !matchesOperators(actual, exp) {
				return false
			}
		case map[string]interface{}:
			// Convert to bson.M and handle as operators
			if !matchesOperators(actual, bson.M(exp)) {
				return false
			}
		default:
			// Simple equality check
			if !isEqual(actual, expected) {
				return false
			}
		}
	}

	return true
}

// matchesOperators handles MongoDB query operators
func matchesOperators(value interface{}, operators bson.M) bool {
	for op, operand := range operators {
		switch op {
		case "$eq":
			if !isEqual(value, operand) {
				return false
			}
		case "$ne":
			if isEqual(value, operand) {
				return false
			}
		case "$gt":
			if !isGreaterThan(value, operand) {
				return false
			}
		case "$gte":
			if !isGreaterThanOrEqual(value, operand) {
				return false
			}
		case "$lt":
			if !isLessThan(value, operand) {
				return false
			}
		case "$lte":
			if !isLessThanOrEqual(value, operand) {
				return false
			}
		case "$in":
			if !isInArray(value, operand) {
				return false
			}
		case "$nin":
			if isInArray(value, operand) {
				return false
			}
		default:
			// Unknown operator, skip (this allows for future compatibility)
			// For strict validation, you could return false here
		}
	}
	return true
}

// applyUpdateOperations applies MongoDB update operators
func applyUpdateOperations(doc bson.M, update bson.M) error {
	for op, fields := range update {
		switch op {
		case "$set":
			switch f := fields.(type) {
			case bson.M:
				for field, value := range f {
					setField(doc, field, value)
				}
			case map[string]interface{}:
				for field, value := range f {
					setField(doc, field, value)
				}
			}
		case "$unset":
			switch f := fields.(type) {
			case bson.M:
				for field := range f {
					unsetField(doc, field)
				}
			case map[string]interface{}:
				for field := range f {
					unsetField(doc, field)
				}
			}
		case "$inc":
			switch f := fields.(type) {
			case bson.M:
				for field, inc := range f {
					incField(doc, field, inc)
				}
			case map[string]interface{}:
				for field, inc := range f {
					incField(doc, field, inc)
				}
			}
		default:
			// Unknown operator, skip
		}
	}
	return nil
}

// setField sets a field value, supporting nested fields
func setField(doc bson.M, field string, value interface{}) {
	parts := strings.Split(field, ".")
	if len(parts) == 1 {
		doc[field] = value
		return
	}
	
	// Navigate to the nested field
	current := doc
	for i := 0; i < len(parts)-1; i++ {
		part := parts[i]
		if next, exists := current[part]; exists {
			if nextMap, ok := next.(bson.M); ok {
				current = nextMap
			} else {
				// Can't navigate further, create new map
				newMap := bson.M{}
				current[part] = newMap
				current = newMap
			}
		} else {
			// Create nested structure
			newMap := bson.M{}
			current[part] = newMap
			current = newMap
		}
	}
	
	// Set the final field
	current[parts[len(parts)-1]] = value
}

// unsetField removes a field, supporting nested fields
func unsetField(doc bson.M, field string) {
	parts := strings.Split(field, ".")
	if len(parts) == 1 {
		delete(doc, field)
		return
	}
	
	// Navigate to the parent of the field to delete
	current := doc
	for i := 0; i < len(parts)-1; i++ {
		part := parts[i]
		if next, exists := current[part]; exists {
			if nextMap, ok := next.(bson.M); ok {
				current = nextMap
			} else {
				return // Can't navigate further
			}
		} else {
			return // Path doesn't exist
		}
	}
	
	// Delete the final field
	delete(current, parts[len(parts)-1])
}

// incField increments a field value
func incField(doc bson.M, field string, inc interface{}) {
	parts := strings.Split(field, ".")
	if len(parts) == 1 {
		if current, exists := doc[field]; exists {
			doc[field] = addNumbers(current, inc)
		} else {
			doc[field] = inc
		}
		return
	}
	
	// Navigate to the field
	current := doc
	for i := 0; i < len(parts)-1; i++ {
		part := parts[i]
		if next, exists := current[part]; exists {
			if nextMap, ok := next.(bson.M); ok {
				current = nextMap
			} else {
				// Can't navigate further, create new map
				newMap := bson.M{}
				current[part] = newMap
				current = newMap
			}
		} else {
			// Create nested structure
			newMap := bson.M{}
			current[part] = newMap
			current = newMap
		}
	}
	
	// Increment the final field
	finalField := parts[len(parts)-1]
	if existing, exists := current[finalField]; exists {
		current[finalField] = addNumbers(existing, inc)
	} else {
		current[finalField] = inc
	}
}

// Helper functions for comparisons
func isEqual(a, b interface{}) bool {
	return fmt.Sprintf("%v", a) == fmt.Sprintf("%v", b)
}

func isGreaterThan(a, b interface{}) bool {
	return compareValues(a, b) > 0
}

func isGreaterThanOrEqual(a, b interface{}) bool {
	return compareValues(a, b) >= 0
}

func isLessThan(a, b interface{}) bool {
	return compareValues(a, b) < 0
}

func isLessThanOrEqual(a, b interface{}) bool {
	return compareValues(a, b) <= 0
}

func isInArray(value, array interface{}) bool {
	switch arr := array.(type) {
	case []interface{}:
		for _, item := range arr {
			if isEqual(value, item) {
				return true
			}
		}
	case primitive.A: // BSON array type
		for _, item := range arr {
			if isEqual(value, item) {
				return true
			}
		}
	case []string:
		valueStr := fmt.Sprintf("%v", value)
		for _, item := range arr {
			if item == valueStr {
				return true
			}
		}
	}
	return false
}

func compareValues(a, b interface{}) int {
	// Simple numeric comparison for MVP
	aFloat := toFloat64(a)
	bFloat := toFloat64(b)
	
	if aFloat > bFloat {
		return 1
	} else if aFloat < bFloat {
		return -1
	}
	return 0
}

func toFloat64(v interface{}) float64 {
	switch val := v.(type) {
	case int:
		return float64(val)
	case int32:
		return float64(val)
	case int64:
		return float64(val)
	case float32:
		return float64(val)
	case float64:
		return val
	default:
		// Try to convert via string
		if str := fmt.Sprintf("%v", v); str != "" {
			if f, err := fmt.Sscanf(str, "%f", new(float64)); err == nil && f == 1 {
				var result float64
				_, _ = fmt.Sscanf(str, "%f", &result)
				return result
			}
		}
		return 0
	}
}

func addNumbers(a, b interface{}) interface{} {
	// Simple addition for MVP
	return toFloat64(a) + toFloat64(b)
}