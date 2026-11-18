// SPDX-License-Identifier: AGPL-3.0-only

package standby

import (
	"testing"
	"time"
)

// FO-12: a freeze and a "gone" server survive a restart of kingresd (a new agent on the same data directory); an
// unfreeze and a clear are remembered too.
func TestAFreezeAndAGoneServerSurviveARestart(t *testing.T) {
	dir := t.TempDir()
	a, err := NewAgent(&Agent{DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	a.Freeze(3, time.Minute)
	a.Freeze(4, time.Minute)
	a.StandbysGone("s2", time.Minute, false)
	a.Unfreeze(4)

	b, err := NewAgent(&Agent{DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if !b.isFrozen(3) {
		t.Error("the freeze of group 3 was forgotten by a restart")
	}
	if b.isFrozen(4) {
		t.Error("an unfrozen group is frozen again after a restart")
	}
	if !b.goneServers()["s2"] {
		t.Error("a server declared gone was forgotten by a restart")
	}
	b.StandbysGone("s2", 0, true)
	c, _ := NewAgent(&Agent{DataDir: dir})
	if c.goneServers()["s2"] {
		t.Error("a cleared server is gone again after a restart")
	}
	if !c.isFrozen(3) {
		t.Error("the freeze was lost when something else changed")
	}
}
