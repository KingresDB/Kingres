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
	for _, p := range []Peer{{"", "h:1"}, {"a", "nohost"}, {"a", ":80"}, {"a", "h:0"}, {"a", "h:99999"}, {"a", "h:x"}} {
		if p.Validate() == nil {
			t.Errorf("%+v accepted", p)
		}
	}
	d, _ := Open(t.TempDir())
	if err := d.Replace([]Peer{{"a", "bad"}}); err == nil {
		t.Error("Replace accepted a bad peer")
	}
}
