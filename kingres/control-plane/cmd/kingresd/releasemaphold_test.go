// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/failover/journal"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/lease"
)

func TestTheWordsOfTheReleaseCommandAreTakenOutOfTheArguments(t *testing.T) {
	for _, c := range []struct {
		args []string
		want []string
		r    releaseHoldArgs
		fail bool
	}{
		{[]string{"-data-dir", "d", "-release-map-hold", "-lost", "s3", "-accept-data-loss"}, []string{"-data-dir", "d"}, releaseHoldArgs{true, "s3", true}, false},
		{[]string{"--release-map-hold", "-lost=s3", "-listen", "x:1"}, []string{"-listen", "x:1"}, releaseHoldArgs{true, "s3", false}, false},
		{[]string{"-listen", "x:1"}, []string{"-listen", "x:1"}, releaseHoldArgs{}, false},
		{[]string{"-lost", "s3"}, nil, releaseHoldArgs{}, true},
		{[]string{"-accept-data-loss"}, nil, releaseHoldArgs{}, true},
		{[]string{"-release-map-hold", "-lost"}, nil, releaseHoldArgs{}, true},
	} {
		rest, r, err := splitReleaseHold(c.args)
		if (err != nil) != c.fail {
			t.Errorf("%v: error %v", c.args, err)
			continue
		}
		if !c.fail && (!reflect.DeepEqual(rest, c.want) && !(len(rest) == 0 && len(c.want) == 0) || r != c.r) {
			t.Errorf("%v: rest %v, words %+v", c.args, rest, r)
		}
	}
}

// holdRig is a data directory of the old leader of the map group whose primary is held back for the candidate s3.
func holdRig(t *testing.T, candidate string) (dir string, args func(extra ...string) []string) {
	t.Helper()
	dir = t.TempDir()
	ld := filepath.Join(dir, leaseDirName)
	if err := os.MkdirAll(ld, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(lease.MarkerPath(ld, lease.RelinquishedMarker, 0), []byte("holding back\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if candidate != "" {
		if err := os.WriteFile(lease.MarkerPath(ld, lease.CandidateMarker, 0), []byte(candidate+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir, func(extra ...string) []string {
		return append([]string{"-data-dir", dir, "-server-host", "s1", "-release-map-hold"}, extra...)
	}
}

func runCommand(args []string) (string, error) {
	var out bytes.Buffer
	err := run(context.Background(), args, noEnv, &out, nil)
	return out.String(), err
}

func auditText(dir string) string {
	b, _ := os.ReadFile(filepath.Join(dir, "audit.jsonl"))
	return string(b)
}

func markers(dir string) (held, candidate, wait bool) {
	ld := filepath.Join(dir, leaseDirName)
	exists := func(base string) bool { _, err := os.Stat(lease.MarkerPath(ld, base, 0)); return err == nil }
	return exists(lease.RelinquishedMarker), exists(lease.CandidateMarker), exists(lease.WaitMarker)
}

// The command ends the hold only with both words, never for the server it runs on, and never while anything else uses the data directory; a refusal changes nothing.
func TestTheReleaseCommandRefusesWhatIsNotSafe(t *testing.T) {
	dir, args := holdRig(t, "s3")
	for name, a := range map[string][]string{
		"no words":         args(),
		"no declaration":   args("-accept-data-loss"),
		"no acceptance":    args("-lost", "s3"),
		"the local server": args("-lost", "s1", "-accept-data-loss"),
	} {
		if _, err := runCommand(a); err == nil {
			t.Errorf("%s: the command ran", name)
		}
	}
	if held, cand, wait := markers(dir); !held || !cand || wait {
		t.Fatalf("a refused command changed the markers: held %v candidate %v wait %v", held, cand, wait)
	}
	if got := auditText(dir); strings.Contains(got, "releaseMapGroupHold") {
		t.Fatalf("a command that did nothing wrote an audit record: %s", got)
	}
	// kingresd (or another command) uses the data directory
	unlock, err := lockDataDir(dir, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runCommand(args("-lost", "s3", "-accept-data-loss")); err == nil || !strings.Contains(err.Error(), "stop kingresd") {
		t.Errorf("the command ran while the data directory was in use: %v", err)
	}
	if _, err := lockDataDir(dir, 0); !errors.Is(err, errDataDirInUse) {
		t.Errorf("a second owner of the data directory: %v", err)
	}
	unlock()
	if held, _, wait := markers(dir); !held || wait {
		t.Fatal("the markers changed while the command was refused")
	}
	// a bootstrap in progress is given up through its own call
	js, err := journal.Open(filepath.Join(dir, "failover"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := js.Begin("b1", "s1", 3, "manual", "alice", false); err != nil {
		t.Fatal(err)
	}
	if _, err := runCommand(args("-lost", "s3", "-accept-data-loss")); err == nil || !strings.Contains(err.Error(), "bootstrap") {
		t.Errorf("the command ran with an active bootstrap: %v", err)
	}
	// the hold was made for another server than the one declared lost
	if err := js.Abandon("x"); err != nil {
		t.Fatal(err)
	}
	if _, err := runCommand(args("-lost", "s4", "-accept-data-loss")); err == nil {
		t.Error("the hold of s3 was released by the declaration that s4 is lost")
	}
	if got := auditText(dir); !strings.Contains(got, `"outcome":"failure"`) {
		t.Errorf("the refused release was not recorded: %s", got)
	}
	if held, cand, wait := markers(dir); !held || !cand || wait {
		t.Fatalf("a refused release changed the markers: held %v candidate %v wait %v", held, cand, wait)
	}
}

// With both words the command ends the hold, leaves the start waiting for the lease, writes an audit record that the audit log keeps, and the lock is free again.
func TestTheReleaseCommandEndsTheHoldAndRecordsIt(t *testing.T) {
	dir, args := holdRig(t, "s3")
	out, err := runCommand(args("-lost", "s3", "-accept-data-loss"))
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if held, cand, wait := markers(dir); held || cand || !wait {
		t.Fatalf("held %v candidate %v wait %v", held, cand, wait)
	}
	got := auditText(dir)
	for _, want := range []string{`"action":"releaseMapGroupHold"`, `"source":"cli"`, `"outcome":"success"`, `"lostServerId":"s3"`, `"acceptDataLoss":true`, `"resourceType":"shardGroup"`} {
		if !strings.Contains(got, want) {
			t.Errorf("the audit record lacks %s: %s", want, got)
		}
	}
	if !strings.Contains(out, "released") {
		t.Errorf("the log line is missing: %s", out)
	}
	if unlock, err := lockDataDir(dir, time.Second); err != nil {
		t.Errorf("the command kept the lock: %v", err)
	} else {
		unlock()
	}
	// nothing is held back any more
	if _, err := runCommand(args("-lost", "s3", "-accept-data-loss")); err == nil {
		t.Error("a second release found a hold")
	}
}
