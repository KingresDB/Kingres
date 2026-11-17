// SPDX-License-Identifier: AGPL-3.0-only

package lease

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// blockingInstance is a fake instance whose Start waits until it is released.
type blockingInstance struct {
	*fakeInstance
	entered chan struct{}
	release chan struct{}
}

func (b *blockingInstance) Start(ctx context.Context) error {
	close(b.entered)
	<-b.release
	return b.fakeInstance.Start(ctx)
}

// ST-04: a fence never waits behind a start of the primary that is under way, and a primary that comes up after the
// fence was written is stopped again: a start must not bring an instance back after a fence stopped it.
func TestAFenceDoesNotWaitForAStartAndTheStartDoesNotSurviveIt(t *testing.T) {
	ctx := context.Background()
	r := newRig(t, []string{"a"}, true)
	bi := &blockingInstance{fakeInstance: &fakeInstance{running: false}, entered: make(chan struct{}), release: make(chan struct{})}
	r.m.c.Instance = bi
	dir := filepath.Join(r.dir, "lease")
	if err := os.WriteFile(FencedPath(dir, 1), []byte("fenced\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r.m.Renew(ctx) // wins the lease and starts the primary (blocked inside Start)
	select {
	case <-bi.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the primary was not started after the lease was won")
	}
	done := make(chan error, 1)
	go func() { done <- r.m.Fence(ctx, 1) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Fence: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the fence waited behind the start")
	}
	if !markerExists(dir, FencedMarker, 1) {
		t.Fatal("the fence wrote no marker")
	}
	close(bi.release)
	r.m.wg.Wait()
	if up, _, _ := bi.state(); up {
		t.Fatal("the instance that came up after the fence is still running")
	}
}
