//go:build !linux

package daemon

import "net"

// verifyUnixPeer checks that the connection arrived over the 0600 Unix domain socket.
func verifyUnixPeer(c net.Conn) bool {
	_, ok := c.(*net.UnixConn)
	return ok
}
