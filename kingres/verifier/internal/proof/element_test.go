// SPDX-License-Identifier: AGPL-3.0-only

package proof

import (
	"io"
	"runtime"
	"runtime/debug"
	"strings"
	"testing"
)

// L12-05: the memory of the verification of a proof is bounded by the size of one
// header whatever the proof holds. One element of the chain of 16,000 strings of
// 65,000 bytes (a gigabyte) was held whole by the reader before the header was
// found to lack its height.

// hostileChain is a proof file that does not end: a chain with one element that
// holds strings for ever. It counts the bytes it was asked for.
type hostileChain struct {
	pos  int
	read int64
}

const hostilePrefix = `{"chain":[{"x":[`

func (h *hostileChain) Read(p []byte) (int, error) {
	unit := `"` + strings.Repeat("A", 65000) + `",`
	for i := range p {
		if h.pos < len(hostilePrefix) {
			p[i] = hostilePrefix[h.pos]
		} else {
			p[i] = unit[(h.pos-len(hostilePrefix))%len(unit)]
		}
		h.pos++
	}
	h.read += int64(len(p))
	return len(p), nil
}

func (h *hostileChain) Close() error { return nil }

func TestAHostileElementIsRefusedAtTheParseStepBeforeItIsHeld(t *testing.T) {
	old := debug.SetGCPercent(20)
	defer debug.SetGCPercent(old)
	h := &hostileChain{}
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	_, _, err := ParseStream(func() (io.ReadCloser, error) { return h, nil })
	runtime.ReadMemStats(&after)
	if err == nil || !strings.Contains(err.Error(), "an element of more than") {
		t.Fatalf("a hostile element: %v", err)
	}
	if h.read > 1<<20 {
		t.Fatalf("%d bytes were read before the element was refused: the reader holds the element", h.read)
	}
	if grown := int64(after.TotalAlloc - before.TotalAlloc); grown > 8<<20 {
		t.Fatalf("%d MiB allocated to refuse an element", grown>>20)
	}
}

// The same bytes are refused by the buffered parse, so the two readings of a
// proof agree about a hostile element.
func TestTheBufferedParseRefusesTheSameElement(t *testing.T) {
	data := hostilePrefix + strings.Repeat(`"`+strings.Repeat("A", 60000)+`",`, 3) + `"z"]}]}`
	if _, err := Parse([]byte(data)); err == nil || !strings.Contains(err.Error(), "an element of more than") {
		t.Fatalf("buffered: %v", err)
	}
	if _, _, err := ParseStream(sourceOf([]byte(data))); err == nil || !strings.Contains(err.Error(), "an element of more than") {
		t.Fatalf("streamed: %v", err)
	}
}

// What is outside the chain is bounded too.
func TestAHostileMemberBesideTheChainIsRefused(t *testing.T) {
	var b strings.Builder
	b.WriteString(`{"chain":[],`)
	for i := 0; i < 30; i++ {
		b.WriteString(`"m` + string(rune('a'+i)) + `":"` + strings.Repeat("A", 60000) + `",`)
	}
	b.WriteString(`"z":0}`)
	if _, _, err := ParseStream(sourceOf([]byte(b.String()))); err == nil || !strings.Contains(err.Error(), "bytes outside the member") {
		t.Fatalf("members beside the chain: %v", err)
	}
}
