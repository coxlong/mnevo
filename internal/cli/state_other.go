//go:build !unix

package cli

import (
	"errors"
	"os"
)

var errUnsupportedPlatform = errors.New("mnevo requires a Unix-like operating system with Unix domain sockets")

func readAdminKey(string) (string, error) {
	return "", errUnsupportedPlatform
}

func ensureStateDirAndKey(string) (*os.File, string, error) {
	return nil, "", errUnsupportedPlatform
}
