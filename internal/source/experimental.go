package source

import (
	"errors"
	"os"
)

var errExperimentalSource = errors.New("this command is experimental and off by default: it connects Argon to a database that is in use. Set ARGON_EXPERIMENTAL_SOURCE=1 to enable it, and read docs/OPERATIONS.md first")

// RequireExperimental checks whether the operator has enabled experimental source features.
func RequireExperimental() error {
	if os.Getenv("ARGON_EXPERIMENTAL_SOURCE") == "1" {
		return nil
	}
	return errExperimentalSource
}
