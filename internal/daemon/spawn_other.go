//go:build !unix

package daemon

import "errors"

func spawnDetachedDaemon(string) error {
	return errors.New("background daemon auto-start requires a Unix-like operating system")
}
