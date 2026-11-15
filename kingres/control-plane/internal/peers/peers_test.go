// SPDX-License-Identifier: PostgreSQL

package peers

import "testing"

func TestDirectoryRoundTrip(t *testing.T) {
	dir := t.TempDir()
	d, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.All()) != 0 {
		t.Fatal("new directory not empty")
	}
	if err := d.Put(Peer{ServerID: "b", InternalAddr: "127.0.0.2:8092"}); err != nil {
		t.Fatal(err)
	}
	if err := d.Put(Peer{ServerID: "a", InternalAddr: "127.0.0.1:8092"}); err != nil {
		t.Fatal(err)
	}
	d2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	all := d2.All()
	if len(all) != 2 || all[0].ServerID != "a" {
		t.Fatalf("reloaded = %+v", all)
	}
	if err := d2.Delete("a"); err != nil {
		t.Fatal(err)
	}
	if _, ok := d2.Get("a"); ok {
		t.Fatal("a still there")
	}
	if err := d2.Delete("zzz"); err != nil {
		t.Fatal(err)
	}
}

func TestValidate(t *testing.T) {
	for _, p := range []Peer{{ServerID: "", InternalAddr: "h:1"}, {ServerID: "a", InternalAddr: "nohost"}, {ServerID: "a", InternalAddr: ":80"}, {ServerID: "a", InternalAddr: "h:0"}, {ServerID: "a", InternalAddr: "h:99999"}, {ServerID: "a", InternalAddr: "h:x"}} {
		if p.Validate() == nil {
			t.Errorf("%+v accepted", p)
		}
	}
	d, _ := Open(t.TempDir())
	if err := d.Replace([]Peer{{ServerID: "a", InternalAddr: "bad"}}); err == nil {
		t.Error("Replace accepted a bad peer")
	}
}

func TestPutKeepsTheOrdinalOfAKnownServer(t *testing.T) {
	d, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Replace([]Peer{{ServerID: "a", InternalAddr: "a:8092", Ordinal: 4}}); err != nil {
		t.Fatal(err)
	}
	// The server records itself at every start, without knowing its ordinal.
	if err := d.Put(Peer{ServerID: "a", InternalAddr: "a:8092"}); err != nil {
		t.Fatal(err)
	}
	if p, _ := d.Get("a"); p.Ordinal != 4 {
		t.Errorf("ordinal %d after Put, want 4", p.Ordinal)
	}
	if err := d.Put(Peer{ServerID: "a", InternalAddr: "a:8092", Ordinal: 5}); err != nil {
		t.Fatal(err)
	}
	if p, _ := d.Get("a"); p.Ordinal != 5 {
		t.Errorf("ordinal %d after a Put with one, want 5", p.Ordinal)
	}
	again, err := Open(d.path[:len(d.path)-len(FileName)])
	if err != nil {
		t.Fatal(err)
	}
	if p, _ := again.Get("a"); p.Ordinal != 5 {
		t.Errorf("ordinal %d after reopening", p.Ordinal)
	}
}
