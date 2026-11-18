package source

import (
	"errors"
	"os"
)

const experimentalEnv = "ARGON_EXPERIMENTAL_SOURCE"

var errExperimentalSource = errors.New("this command is experimental and off by default: it connects Argon to a database that is in use. Set ARGON_EXPERIMENTAL_SOURCE=1 to enable it, and read docs/OPERATIONS.md first")
var errExperimentalImages = errors.New("--enable-source-images is experimental and off by default. Set ARGON_EXPERIMENTAL_SOURCE=1 to use it, or enable changeStreamPreAndPostImages directly in MongoDB")

// RequireExperimental checks whether the operator has enabled experimental source features.
func RequireExperimental() error {
	if os.Getenv(experimentalEnv) == "1" {
		return nil
	}
	return errExperimentalSource
}

// RequireExperimentalImages checks the separate opt-in for altering source collections.
func RequireExperimentalImages() error {
	if os.Getenv(experimentalEnv) == "1" {
		return nil
	}
	return errExperimentalImages
}
