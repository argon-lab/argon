package walcli

import "github.com/argon-lab/argon/v2/internal/dberr"

// IsUnauthorized reports whether MongoDB refused an operation for lack of a
// privilege: Unauthorized (13) on a self-managed deployment, AtlasError 8000
// "not allowed to do action" on MongoDB Atlas.
func IsUnauthorized(err error) bool { return dberr.Unauthorized(err) }
