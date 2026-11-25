// SPDX-License-Identifier: AGPL-3.0-only

package standby

import (
	"errors"
	"testing"
)

func TestTheMetaFileFollowsThePortOfTheMarker(t *testing.T) {
	var written []Entry
	write := func(id int, e Entry) error { written = append(written, e); return nil }
	portOf := func(p int, ok bool) func() (int, bool) { return func() (int, bool) { return p, ok } }

	// A marker with another port than the meta file (a re-pick that was cut short before the meta was written): rewritten,
	// whatever the instance is doing, so the supervision round does it for a running standby too.
	e, err := syncMetaPort(portOf(57077, true), 101, Entry{ID: 5, Port: 57002}, write)
	if err != nil || e.Port != 57077 || len(written) != 1 || written[0].Port != 57077 || written[0].ID != 5 {
		t.Fatalf("%+v %v %+v", e, err, written)
	}
	// Equal ports: nothing is written.
	written = nil
	if _, err := syncMetaPort(portOf(57077, true), 101, Entry{Port: 57077}, write); err != nil || len(written) != 0 {
		t.Fatalf("%v %+v", err, written)
	}
	// A marker without a port (the copy is not complete) or none at all: the meta file is left alone.
	for _, c := range []func() (int, bool){portOf(0, true), portOf(0, false), portOf(57077, false)} {
		if e, err := syncMetaPort(c, 101, Entry{Port: 57002}, write); err != nil || e.Port != 57002 || len(written) != 0 {
			t.Fatalf("%+v %v %+v", e, err, written)
		}
	}
	// A failed write is an error that names the port.
	if _, err := syncMetaPort(portOf(57077, true), 101, Entry{Port: 1}, func(int, Entry) error { return errors.New("disk full") }); err == nil {
		t.Fatal("a failed write must be reported")
	}
}
