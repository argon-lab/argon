package dberr

import (
	"errors"
	"fmt"
	"testing"

	"go.mongodb.org/mongo-driver/mongo"
)

func TestUnauthorized(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"server Unauthorized", mongo.CommandError{Code: 13, Name: "Unauthorized", Message: "not authorized on db to execute command"}, true},
		{"Atlas shared tier refusal", mongo.CommandError{Code: 8000, Name: "AtlasError", Message: "user is not allowed to do action [listIndexes] on [db.orders]"}, true},
		{"wrapped", fmt.Errorf("list indexes of orders: %w", mongo.CommandError{Code: 8000, Name: "AtlasError", Message: "user is not allowed to do action [find] on [db.c]"}), true},
		{"write exception", mongo.WriteException{WriteErrors: []mongo.WriteError{{Code: 13, Message: "not authorized"}}}, true},
		{"other Atlas error", mongo.CommandError{Code: 8000, Name: "AtlasError", Message: "CMD_NOT_ALLOWED: createUser"}, false},
		{"other server error", mongo.CommandError{Code: 11000, Message: "E11000 duplicate key"}, false},
		{"not a server error", errors.New("not authorized"), false},
		{"nil", nil, false},
	}
	for _, c := range cases {
		if got := Unauthorized(c.err); got != c.want {
			t.Errorf("%s: Unauthorized = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestAtlasRestriction(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"collection limit", mongo.CommandError{Code: 8000, Name: "AtlasError", Message: "cannot create a new collection -- already using 500 collections of 500"}, true},
		{"space quota, wrapped", fmt.Errorf("collection orders: %w", mongo.CommandError{Code: 8000, Name: "AtlasError", Message: "you are over your space quota, using 513 MB of 512 MB"}), true},
		{"bulk write over quota", mongo.BulkWriteException{WriteErrors: []mongo.BulkWriteError{{WriteError: mongo.WriteError{Code: 8000, Message: "you are over your space quota"}}}}, true},
		{"missing privilege is not a restriction", mongo.CommandError{Code: 8000, Name: "AtlasError", Message: "user is not allowed to do action [dropDatabase] on [x.]"}, false},
		{"unsupported command is not a restriction", mongo.CommandError{Code: 8000, Name: "AtlasError", Message: "CMD_NOT_ALLOWED: createUser"}, false},
		{"other server error", mongo.CommandError{Code: 11000, Message: "E11000 duplicate key"}, false},
		{"nil", nil, false},
	}
	for _, c := range cases {
		if got := AtlasRestriction(c.err); got != c.want {
			t.Errorf("%s: AtlasRestriction = %v, want %v", c.name, got, c.want)
		}
	}
}
