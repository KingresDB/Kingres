// SPDX-License-Identifier: AGPL-3.0-only

package ledgerround

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/internalapi"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/ledgerpub"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/peerclient"
)

// The files of the publication on the leader and through a follower: the leader reads its store, the follower asks the leader over the
// internal API and keeps only the immutable objects.
func TestAFollowerReadsThroughTheLeaderAndCachesOnlyImmutableFiles(t *testing.T) {
	e := newEnv(t, newChain(1, 300))
	e.round()
	l := e.chainLog(0)
	logPath := l.Origin[len(testPrefix)+1:]
	leaderFiles := &Files{Store: e.store, Prefix: func(context.Context) (string, error) { return testPrefix, nil },
		Leader: func(context.Context) (string, bool, error) { return "srv0", true, nil }}
	calls := 0
	follower := &Files{Store: nil, CacheBytes: 1 << 20,
		Leader: func(context.Context) (string, bool, error) { return "srv0", false, nil },
		Call: func(ctx context.Context, server, method, path string, in, out any) error {
			calls++
			if server != "srv0" || method != "POST" || path != "/ledger/publication/get" {
				t.Errorf("call %s %s %s", server, method, path)
			}
			body, _ := json.Marshal(in)
			res, err := leaderFiles.Handle(ctx, body)
			if err != nil {
				var ce *internalapi.CallError
				if errors.As(err, &ce) {
					return &peerclient.APIError{Status: ce.Status, Code: ce.Code, Message: ce.Message}
				}
				return err
			}
			b, _ := json.Marshal(res)
			return json.Unmarshal(b, out)
		}}
	get := func(f *Files, p string) (ledgerpub.Object, error) {
		lp, err := ledgerpub.ParsePath(p)
		if err != nil {
			t.Fatal(err)
		}
		return f.Objects(bg, lp)
	}
	for _, p := range []string{logPath + "/tile/0/000", logPath + "/tile/entries/000", logPath + "/checkpoint"} {
		a, err := get(leaderFiles, p)
		if err != nil {
			t.Fatalf("leader %s: %v", p, err)
		}
		b, err := get(follower, p)
		if err != nil || string(a.Data) != string(b.Data) || a.Mutable != b.Mutable || a.ContentType != b.ContentType {
			t.Fatalf("follower %s: %v (mutable %v/%v)", p, err, a.Mutable, b.Mutable)
		}
	}
	if calls != 3 {
		t.Fatalf("%d calls", calls)
	}
	// an immutable tile is kept, the checkpoint is asked for again
	get(follower, logPath+"/tile/0/000")
	get(follower, logPath+"/checkpoint")
	if calls != 4 {
		t.Errorf("calls %d: the tile must come from the cache and the checkpoint must not", calls)
	}
	// a file that is not there is a not found, on the leader and through the follower
	if _, err := get(follower, logPath+"/tile/0/005"); !errors.Is(err, ledgerpub.ErrNotFound) {
		t.Errorf("a missing tile: %v", err)
	}
	if _, err := get(leaderFiles, "events/000000"); !errors.Is(err, ledgerpub.ErrNotFound) {
		t.Errorf("a missing event: %v", err)
	}
	// the call is answered by the leader only, and refuses a path that is not the layout
	notLeader := &Files{Store: e.store, Leader: func(context.Context) (string, bool, error) { return "srv0", false, nil }}
	if _, err := notLeader.Handle(bg, []byte(`{"path":"keyring.json"}`)); err == nil {
		t.Error("a follower answered the call")
	} else {
		var ce *internalapi.CallError
		if !errors.As(err, &ce) || ce.Status != http.StatusConflict {
			t.Errorf("%v", err)
		}
	}
	for _, body := range []string{`{"path":"../keyring.json"}`, `{"path":""}`, `{"path":"c/zz/e1/checkpoint"}`, `nonsense`} {
		if _, err := leaderFiles.Handle(bg, []byte(body)); err == nil {
			t.Errorf("%s was answered", body)
		}
	}
}
