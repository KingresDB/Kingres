// SPDX-License-Identifier: AGPL-3.0-only

package pgframe

import (
	"encoding/binary"
	"errors"
	"io"
	"net"
	"testing"
)

func message(typ byte, body int) []byte {
	b := make([]byte, 5+body)
	b[0] = typ
	binary.BigEndian.PutUint32(b[1:], uint32(body+4))
	return b
}

// readAll reads from the wrapped end of a pipe, written in the given chunks.
func readAll(t *testing.T, max int, chunks ...[]byte) ([]byte, error) {
	t.Helper()
	a, b := net.Pipe()
	go func() {
		for _, c := range chunks {
			if _, err := b.Write(c); err != nil {
				return
			}
		}
		_ = b.Close()
	}()
	return io.ReadAll(Limit(a, max))
}

func TestMessagesWithinTheLimitPass(t *testing.T) {
	stream := append(append(message('D', 100), message('C', 0)...), message('Z', 1)...)
	got, err := readAll(t, 100, stream)
	if err != nil || len(got) != len(stream) {
		t.Fatalf("read %d of %d bytes: %v", len(got), len(stream), err)
	}
}

func TestAHeaderSplitAcrossReadsIsStillSeen(t *testing.T) {
	m := message('D', 5000)
	if _, err := readAll(t, 1000, m[:2], m[2:4], m[4:]); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("err = %v, want ErrTooLarge", err)
	}
}

func TestAnOversizedMessageIsRefusedBeforeItsBodyIsDelivered(t *testing.T) {
	small := message('D', 10)
	big := message('D', 1<<20)
	got, err := readAll(t, 1<<10, append(small, big...))
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("err = %v, want ErrTooLarge", err)
	}
	if len(got) > len(small) {
		t.Errorf("%d bytes were delivered, the small message has %d", len(got), len(small))
	}
}
