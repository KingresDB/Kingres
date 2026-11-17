// SPDX-License-Identifier: AGPL-3.0-only

package membership

import (
	"testing"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/peers"
)

// The leader's copy of the directory keeps a server that another task is adding
// right now when a finishing task writes the list it read a moment before.
func TestRecordDirectoryKeepsAServerThatIsBeingAdded(t *testing.T) {
	dir, err := peers.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	m := &Manager{Peers: dir}
	_ = dir.Put(peers.Peer{ServerID: "a", InternalAddr: "a:1"})
	_ = dir.Put(peers.Peer{ServerID: "c", InternalAddr: "c:1"}) // pending: introduced after the list below was read
	list := []peers.Peer{{ServerID: "a", InternalAddr: "a:1", Ordinal: 1}, {ServerID: "b", InternalAddr: "b:1", Ordinal: 2}}
	if err := m.recordDirectory(list, ""); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"a", "b", "c"} {
		if _, ok := dir.Get(id); !ok {
			t.Errorf("%s is missing from the directory", id)
		}
	}
	if err := m.recordDirectory(list, "b"); err != nil {
		t.Fatal(err)
	}
	if _, ok := dir.Get("b"); ok {
		t.Error("the removed server is still in the directory")
	}
}

// A step that finds the server it works on missing from the directory puts it back.
func TestEnsurePeerRestoresTheAddressFromTheTaskInput(t *testing.T) {
	dir, _ := peers.Open(t.TempDir())
	m := &Manager{Peers: dir}
	m.ensurePeer(addInput{ServerID: "n", InternalAddress: "n:2"})
	if p, ok := dir.Get("n"); !ok || p.InternalAddr != "n:2" {
		t.Fatalf("directory: %v %v", p, ok)
	}
}
