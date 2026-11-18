package wal_test

import (
	"context"
	"encoding/binary"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/argon-lab/argon/v2/pkg/walcli"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

func TestOnlineImport_ProbesTheReplayStreamBeforeCopying(t *testing.T) {
	sourceURI := os.Getenv("ARGON_TEST_SOURCE_MONGODB_URI")
	if sourceURI == "" {
		t.Skip("needs ARGON_TEST_SOURCE_MONGODB_URI")
	}
	hosts := options.Client().ApplyURI(sourceURI).Hosts
	require.NotEmpty(t, hosts)
	unauthorized := mongo.CommandError{Code: 13, Name: "Unauthorized", Message: "not authorized to open a change stream"}
	for _, test := range []struct {
		name         string
		dryRun       bool
		refusal      mongo.CommandError
		errorMessage string
	}{
		{name: "same options and start position"},
		{
			name:         "expanded events unsupported",
			refusal:      mongo.CommandError{Code: 40415, Name: "Location40415", Message: "BSON field '$changeStream.showExpandedEvents' is an unknown field"},
			errorMessage: "does not support the change-stream options Argon follows with",
		},
		{
			name:         "change stream privilege missing",
			refusal:      unauthorized,
			errorMessage: "the source user needs the changeStream action",
		},
		{
			name:         "dry run with change stream privilege missing",
			dryRun:       true,
			refusal:      unauthorized,
			errorMessage: "the source user needs the changeStream action",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newFollowFixture(t, sourceURI)
			f.seedSmall(t)
			var mu sync.Mutex
			var opens []streamOpen
			copyReads := 0
			addr := startSourceCommandProxy(t, hosts[0], func(message []byte) ([]byte, bool) {
				body := sourceWireCommand(message)
				if db, _ := body.Lookup("$db").StringValueOK(); db != f.source.Name() {
					return nil, false
				}
				mu.Lock()
				defer mu.Unlock()
				if name, _ := body.Lookup("find").StringValueOK(); name == "catalog" {
					// Preflight checks permission with a single-document find.
					if singleBatch, _ := body.Lookup("singleBatch").BooleanOK(); !singleBatch {
						copyReads++
					}
				}
				if spec, ok := body.Lookup("pipeline", "0", "$changeStream").DocumentOK(); ok {
					batchSize, _ := body.Lookup("cursor", "batchSize").Int32OK()
					opens = append(opens, streamOpen{spec: spec, batchSize: batchSize, copyReads: copyReads})
					expanded, _ := spec.Lookup("showExpandedEvents").BooleanOK()
					if test.refusal.Code == 13 || (test.refusal.Code == 40415 && expanded) {
						return sourceCommandErrorReply(message, test.refusal), false
					}
				}
				return nil, false
			})
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			_, err := f.services.ImportDatabaseOnline(ctx, walcli.OnlineImport{
				URI: "mongodb://" + addr + "/?directConnection=true", Database: f.source.Name(), Project: f.project, DryRun: test.dryRun,
			})
			mu.Lock()
			defer mu.Unlock()
			if test.refusal.Code != 0 {
				assert.Zero(t, copyReads, "the copy must not start when the stream probe fails")
				assert.ErrorContains(t, err, "the source's change stream cannot be opened")
				assert.ErrorContains(t, err, test.errorMessage)
				assert.Len(t, opens, 1)
				_, err = f.services.Projects.GetProjectByName(f.project)
				require.ErrorContains(t, err, "project not found")
				return
			}
			require.NoError(t, err)
			require.Len(t, opens, 2)
			require.Zero(t, opens[0].copyReads)
			require.Positive(t, opens[1].copyReads)
			require.Equal(t, opens[1].spec.String(), opens[0].spec.String(), "probe and replay use the same options and start position")
			require.Positive(t, opens[1].batchSize)
			require.Equal(t, opens[1].batchSize, opens[0].batchSize)
			require.True(t, opens[0].spec.Lookup("showExpandedEvents").Boolean())
			require.Equal(t, "whenAvailable", opens[0].spec.Lookup("fullDocument").StringValue())
			require.Equal(t, "whenAvailable", opens[0].spec.Lookup("fullDocumentBeforeChange").StringValue())
		})
	}
}

type streamOpen struct {
	spec      bson.Raw
	batchSize int32
	copyReads int
}

const mongoOpMsg = 2013

func sourceWireCommand(message []byte) bson.Raw {
	if len(message) < 26 || binary.LittleEndian.Uint32(message[12:16]) != mongoOpMsg || message[20] != 0 {
		return nil
	}
	size := int(binary.LittleEndian.Uint32(message[21:25]))
	if size < 5 || size > len(message)-21 {
		return nil
	}
	return bson.Raw(message[21 : 21+size])
}

func sourceCommandErrorReply(request []byte, refusal mongo.CommandError) []byte {
	body, _ := bson.Marshal(bson.D{
		{Key: "ok", Value: float64(0)}, {Key: "errmsg", Value: refusal.Message},
		{Key: "code", Value: refusal.Code}, {Key: "codeName", Value: refusal.Name},
	})
	reply := make([]byte, 21, 21+len(body))
	binary.LittleEndian.PutUint32(reply[0:4], uint32(21+len(body)))
	binary.LittleEndian.PutUint32(reply[4:8], 1)
	copy(reply[8:12], request[4:8])
	binary.LittleEndian.PutUint32(reply[12:16], mongoOpMsg)
	return append(reply, body...)
}
