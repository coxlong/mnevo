//go:build linux

package daemon

import (
	"net"
	"os"
	"syscall"
)

// verifyUnixPeer verifies via Linux SO_PEERCRED that the peer UID matches the daemon's euid.
func verifyUnixPeer(c net.Conn) bool {
	uc, ok := c.(*net.UnixConn)
	if !ok {
		return false
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		return false
	}
	var cred *syscall.Ucred
	var credErr error
	if err := raw.Control(func(fd uintptr) {
		cred, credErr = syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
	}); err != nil || credErr != nil || cred == nil {
		return false
	}
	return int(cred.Uid) == os.Geteuid() && cred.Pid > 0
}
