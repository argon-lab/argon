// Package dberr classifies MongoDB server errors that more than one part of
// Argon has to recognise.
package dberr

import (
	"errors"

	"go.mongodb.org/mongo-driver/mongo"
)

// atlasError is the code MongoDB Atlas's shared tiers (M0, Flex) return for
// commands and privileges they refuse, in place of the server's own codes.
const atlasError = 8000

// Unauthorized reports a server refusal for a missing privilege. A MongoDB
// server answers Unauthorized (13). Atlas's shared tiers answer AtlasError
// (8000) with "user is not allowed to do action [...]" instead.
func Unauthorized(err error) bool {
	var server mongo.ServerError
	if !errors.As(err, &server) {
		return false
	}
	return server.HasErrorCode(13) ||
		server.HasErrorCode(atlasError) && server.HasErrorMessage("not allowed to do action")
}

// AtlasRestriction reports whether MongoDB Atlas refused an operation for a
// reason other than a missing privilege or an unsupported command: on the
// free and Flex tiers this is how a tier limit is reported (databases,
// collections, storage, data transfer), with the usage in the message.
func AtlasRestriction(err error) bool {
	var server mongo.ServerError
	if !errors.As(err, &server) || !server.HasErrorCode(atlasError) {
		return false
	}
	return !server.HasErrorMessage("not allowed to do action") && !server.HasErrorMessage("CMD_NOT_ALLOWED")
}
