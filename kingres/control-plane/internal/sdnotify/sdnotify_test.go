// SPDX-License-Identifier: PostgreSQL

package sdnotify

import (
	"net"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestNotifyNoop(t *testing.T) {
	if ok, err := Notify("", "READY=1"); ok || err != nil {
		t.Errorf("empty socket: %v %v", ok, err)
	}
	if ok, err := Notify("relative", "READY=1"); ok || err != nil {
		t.Errorf("relative socket: %v %v", ok, err)
	}
}

func TestNotifySends(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix datagram sockets are tested on Linux")
	}
	p := filepath.Join(t.TempDir(), "n.sock")
	l, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: p, Net: "unixgram"})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if ok, err := Notify(p, "READY=1"); !ok || err != nil {
		t.Fatalf("%v %v", ok, err)
	}
	buf := make([]byte, 64)
	_ = l.SetReadDeadline(time.Now().Add(time.Second))
	n, _, err := l.ReadFromUnix(buf)
	if err != nil || string(buf[:n]) != "READY=1" {
		t.Errorf("got %q %v", buf[:n], err)
	}
}
