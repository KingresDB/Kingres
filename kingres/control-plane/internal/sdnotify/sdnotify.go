// SPDX-License-Identifier: PostgreSQL

// Package sdnotify implements the part of the systemd notification protocol
// that kingresd needs (READY=1, STOPPING=1) with the standard library only.
// It does nothing when NOTIFY_SOCKET is not set.
package sdnotify

import "net"

// Notify sends state to the socket named by the environment variable value
// socket (the value of NOTIFY_SOCKET). It reports whether a message was sent.
// Only filesystem and abstract unix datagram sockets are accepted.
func Notify(socket, state string) (bool, error) {
	if socket == "" {
		return false, nil
	}
	if socket[0] == '@' {
		socket = "\x00" + socket[1:]
	} else if socket[0] != '/' {
		return false, nil
	}
	c, err := net.DialUnix("unixgram", nil, &net.UnixAddr{Name: socket, Net: "unixgram"})
	if err != nil {
		return false, err
	}
	defer c.Close()
	if _, err := c.Write([]byte(state)); err != nil {
		return false, err
	}
	return true, nil
}
