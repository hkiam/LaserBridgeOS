//go:build !linux

package proxy

import (
	"errors"
	"net"
)

// peerUID has no answer away from Linux. The appliance is Linux; this exists so
// that the package still builds in an editor on a laptop.
func peerUID(net.Conn) (uint32, error) {
	return 0, errors.New("peer credentials are only available on Linux")
}
