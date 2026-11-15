// SPDX-License-Identifier: PostgreSQL

// Package pgframe bounds the size of one protocol message that a PostgreSQL
// server may send to kingresd. pgx reads a whole message into memory before it
// hands it out (a DataRow with a value of several hundred megabytes included),
// and its client has no limit of its own, so the limit is enforced on the byte
// stream: the connection is closed as soon as a message header announces a body
// larger than the limit, before any of that body is read.
package pgframe

import (
	"context"
	"encoding/binary"
	"errors"
	"net"
)

// ErrTooLarge is returned by a read when the server announced a message larger
// than the limit. The connection is closed.
var ErrTooLarge = errors.New("pgframe: the server sent a message larger than the limit")

// Dialer returns a function for pgconn.Config.DialFunc that dials like net.Dialer and limits the body of
// every message the server sends to max bytes.
func Dialer(max int) func(ctx context.Context, network, addr string) (net.Conn, error) {
	d := &net.Dialer{}
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		c, err := d.DialContext(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		return Limit(c, max), nil
	}
}

// Limit wraps c so that a message with a body larger than max bytes fails the
// read that sees its header (the bytes of that read are not delivered).
func Limit(c net.Conn, max int) net.Conn { return &limited{Conn: c, max: int64(max)} }

type limited struct {
	net.Conn
	max int64
	// hdr collects the five bytes of a message header (type, length) that a
	// read may deliver in pieces; left is how much of the current body has
	// still to pass.
	hdr  [5]byte
	have int
	left int64
}

func (l *limited) Read(p []byte) (int, error) {
	n, err := l.Conn.Read(p)
	for i := 0; i < n; {
		if l.left > 0 {
			skip := int64(n - i)
			if skip > l.left {
				skip = l.left
			}
			l.left -= skip
			i += int(skip)
			continue
		}
		l.hdr[l.have] = p[i]
		l.have++
		i++
		if l.have < len(l.hdr) {
			continue
		}
		l.have = 0
		// The length counts its own four bytes.
		l.left = int64(binary.BigEndian.Uint32(l.hdr[1:])) - 4
		if l.left > l.max {
			_ = l.Conn.Close()
			return 0, ErrTooLarge
		}
		if l.left < 0 {
			l.left = 0
		}
	}
	return n, err
}

// MaxRoleMessage is the largest message accepted on the connections that
// run statements of signed-in roles (the workbench, inclusion proofs): the
// result budget of the workbench. A larger row cannot be returned anyway.
const MaxRoleMessage = 16 << 20
