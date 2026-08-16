//go:build linux

package proxy

import (
	"errors"
	"net"
	"syscall"
)

// peerUID asks the kernel who is at the other end of a Unix socket.
//
// The status socket is world-writable on purpose: the status is not a secret,
// and the operator's own "laserbridge grbl-status" should not need doas to ask.
// Commands are a different matter - they move a machine and can switch a laser
// on - so they are refused to anyone but root, which on this appliance means
// the web backend and nothing else.
//
// On this appliance the distinction is currently theoretical: there is one
// account and it may use doas without a password. It is here because "the
// status is not a secret" was the whole argument for the socket's permissions,
// and that argument does not carry over to steering.
func peerUID(conn net.Conn) (uint32, error) {
	unixConn, ok := conn.(*net.UnixConn)
	if !ok {
		return 0, errors.New("not a unix socket")
	}
	raw, err := unixConn.SyscallConn()
	if err != nil {
		return 0, err
	}
	var credentials *syscall.Ucred
	var lookupErr error
	if err := raw.Control(func(fd uintptr) {
		credentials, lookupErr = syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
	}); err != nil {
		return 0, err
	}
	if lookupErr != nil {
		return 0, lookupErr
	}
	return credentials.Uid, nil
}
