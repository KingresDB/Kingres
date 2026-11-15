// SPDX-License-Identifier: PostgreSQL

package fault

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestNilHookInjectsNothing(t *testing.T) {
	var h *Hook
	if h.OutboundBlocked("a") || h.InboundBlocked("a") || h.ClockOffset() != 0 {
		t.Fatal("a nil hook must inject nothing")
	}
	if New("") != nil {
		t.Fatal("an empty directory must give a nil hook")
	}
}

func TestHookFollowsTheFile(t *testing.T) {
	dir := t.TempDir()
	h := New(dir)
	at := time.Unix(1000, 0)
	h.now = func() time.Time { return at }
	if h.OutboundBlocked("b") {
		t.Fatal("no file, no fault")
	}
	write := func(s string) {
		if err := os.WriteFile(filepath.Join(dir, FileName), []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
		at = at.Add(time.Second)
	}
	write(`{"blockOutbound":["b"],"blockInbound":["*"],"clockOffsetMs":5000}`)
	if !h.OutboundBlocked("b") || h.OutboundBlocked("c") || !h.InboundBlocked("anyone") || h.ClockOffset() != 5*time.Second {
		t.Fatal("the file was not applied")
	}
	write(`{`) // half written
	if h.OutboundBlocked("b") {
		t.Fatal("an unreadable file means no faults")
	}
}
