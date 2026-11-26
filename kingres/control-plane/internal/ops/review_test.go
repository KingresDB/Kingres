// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package ops

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/api"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/problem"
)

func TestCompareVersionsAsNumbers(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want int
	}{
		{"1.0", "1.0", 0}, {"0.10", "0.9", 1}, {"0.9", "0.10", -1}, {"1.0", "0.2", 1}, {"1.0.1", "1.0", 1}, {"1.0", "1.0.0", 0}, {"", "0.1", -1}, {"0.1", "", 1},
		{"1.0b", "1.0a", 1}, {"2", "10", -1},
	} {
		if got := compareVersions(c.a, c.b); got != c.want {
			t.Errorf("compareVersions(%q, %q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestSecondPartial(t *testing.T) {
	if !secondPartial("partial", nil) {
		t.Error("two partial runs in a row")
	}
	for _, c := range []struct {
		prev string
		err  error
	}{{"done", nil}, {"failed", nil}, {"", nil}, {"partial", errors.New("no rows")}} {
		if secondPartial(c.prev, c.err) {
			t.Errorf("%+v counted", c)
		}
	}
}

// A sort order is for a partition: on a table it is refused, and for heap too.
func TestValidateStorageSortByBelongsToAPartition(t *testing.T) {
	_, err := validateStorage(api.TableStorageRequest{AccessMethod: "columnar", SortBy: []string{"ts"}}, false)
	var p *problem.Problem
	if !errors.As(err, &p) || p.Code != problem.ValidationFailed || p.Errors[0].Field != "/sortBy" {
		t.Errorf("sortBy on a table: %v", err)
	}
	if _, err := validateStorage(api.TableStorageRequest{AccessMethod: "columnar", SortBy: []string{"ts"}}, true); err != nil {
		t.Errorf("sortBy on a partition: %v", err)
	}
	if _, err := validateStorage(api.TableStorageRequest{AccessMethod: "heap", SortBy: []string{"ts"}}, true); err == nil {
		t.Error("sortBy with heap accepted")
	}
}

func TestConversionRefusedCarriesTypedBlockers(t *testing.T) {
	err := conversionRefused([]api.TableBlocker{{Code: "foreign_keys", Object: "public.t", Message: "it has foreign keys"}, {Code: "publication", Object: "public.t", Message: "it belongs to a publication"}})
	var p *problem.Problem
	if !errors.As(err, &p) || p.Code != problem.ConversionRefused {
		t.Fatalf("%v", err)
	}
	bl, ok := p.Blockers.([]api.TableBlocker)
	if !ok || len(bl) != 2 || bl[0].Code != "foreign_keys" || p.Detail != "The storage format cannot be changed: it has foreign keys; it belongs to a publication." {
		t.Errorf("blockers %v detail %q", p.Blockers, p.Detail)
	}
}

func TestNotOnThisInstanceSaysTheLimit(t *testing.T) {
	var p *problem.Problem
	if !errors.As(notOnThisInstance("public", "orders"), &p) || p.Code != problem.NotFound {
		t.Fatal("not a 404")
	}
	for _, want := range []string{"public.orders", "instance of the manager leader", "another shard group"} {
		if !strings.Contains(p.Detail, want) {
			t.Errorf("the detail %q does not say %q", p.Detail, want)
		}
	}
}

func TestMaintainTimingsHaveSaneDefaults(t *testing.T) {
	if maintainSkew != 5*time.Minute || maintainBackoff != 5*time.Minute {
		t.Errorf("skew %v backoff %v", maintainSkew, maintainBackoff)
	}
	// P12D-05: a reader of the table waits behind a conversion that asks for its lock for this long at a time, so it stays short
	if storageLockTimeout > 500*time.Millisecond || maintainHorizon != 7*24*time.Hour {
		t.Errorf("lock timeout %v horizon %v", storageLockTimeout, maintainHorizon)
	}
}

// A conversion to columnar needs the library loaded on every server of the cluster, not on the leader only: a standby that cannot replay the WAL of the
// resource manager would stop. The check names the server that has not loaded it; a conversion to heap, and a server that has, are not refused.
func TestStorageNeedsTheLibraryLoadedOnEveryServer(t *testing.T) {
	c := newFakeCluster("a", "b", "c")
	c.ext["c"].preloaded = map[string]bool{} // c was started before the library was added to its list
	o := c.ops["a"]
	p := storageParams{Database: "shop", Schema: "public", Table: "t", AccessMethod: "columnar"}
	err := o.ensureStorageExtensions(context.Background(), p, true)
	var pr *problem.Problem
	if !errors.As(err, &pr) || pr.Code != problem.ExtensionMissing || !strings.Contains(pr.Detail, "server c") || !strings.Contains(pr.Detail, "restart") {
		t.Fatalf("%v", err)
	}
	if strings.Contains(pr.Detail, "server b:") {
		t.Errorf("b has it: %q", pr.Detail)
	}
	p.AccessMethod = "heap"
	if err := o.ensureStorageExtensions(context.Background(), p, true); err != nil {
		t.Errorf("a conversion to heap does not need the library on the others: %v", err)
	}
	c.ext["c"].preloaded[ExtColumnar] = true
	p.AccessMethod = "columnar"
	if err := o.ensureStorageExtensions(context.Background(), p, true); err != nil {
		t.Errorf("every server has it: %v", err)
	}
	// a server that is the leader's own: the leader's instance without the library is refused first
	c.ext["a"].preloaded = map[string]bool{}
	if err := o.ensureStorageExtensions(context.Background(), p, true); err == nil || !errors.As(err, &pr) || !strings.Contains(pr.Detail, "server a") {
		t.Errorf("the leader's own instance: %v", err)
	}
}
