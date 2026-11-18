package source

import (
	"errors"
	"os"
)

const experimentalEnv = "ARGON_EXPERIMENTAL_SOURCE"

var errExperimentalWriteBack = errors.New("this command is experimental and off by default: it connects Argon to a database that is in use. Set ARGON_EXPERIMENTAL_SOURCE=1 to enable it, and read docs/OPERATIONS.md first")
var errExperimentalImages = errors.New("--enable-source-images is experimental and off by default. Set ARGON_EXPERIMENTAL_SOURCE=1 to use it, or enable changeStreamPreAndPostImages directly in MongoDB")
var errExperimentalAutoResync = errors.New("--auto-resync is experimental and off by default. Set ARGON_EXPERIMENTAL_SOURCE=1 to allow following to resync main automatically")

// RequireExperimentalWriteBack checks the opt-in for writing a merge plan to the source.
func RequireExperimentalWriteBack() error {
	if os.Getenv(experimentalEnv) == "1" {
		return nil
	}
	return errExperimentalWriteBack
}

// RequireExperimentalSourceImages checks the opt-in for altering source collections.
func RequireExperimentalSourceImages() error {
	if os.Getenv(experimentalEnv) == "1" {
		return nil
	}
	return errExperimentalImages
}

// RequireExperimentalAutoResync checks the opt-in for automatic repair of main.
func RequireExperimentalAutoResync() error {
	if os.Getenv(experimentalEnv) == "1" {
		return nil
	}
	return errExperimentalAutoResync
}
