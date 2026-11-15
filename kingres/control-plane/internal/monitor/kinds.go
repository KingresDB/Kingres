// SPDX-License-Identifier: AGPL-3.0-only

package monitor

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// Thresholds of the checks added with contract 0.5.0.
const (
	// LedgerPendingMaxAge is how long the oldest pending ledger transaction of
	// a database may wait for its block before ledgerPendingTooOld is raised
	// (the block builder runs every ten seconds by default).
	LedgerPendingMaxAge = 5 * time.Minute
	// CAWarnRemaining and CACritRemaining are the time left of the cluster CA
	// certificate at which certificateExpiring is raised for it: the CA is not
	// rotated by this version, so an expiring one needs an operator.
	CAWarnRemaining = 365 * 24 * time.Hour
	CACritRemaining = 30 * 24 * time.Hour
)

// CertificateReading is a certificate to check: Remaining is the part of its
// lifetime that is left (0 to 1), NotAfter its end, RenewFraction the part of
// the lifetime at which the renewal job renews it, Now the time of the reading.
// CA is true for the cluster CA certificate, which is judged by the time left.
type CertificateReading struct {
	Remaining     float64
	NotAfter      time.Time
	RenewFraction float64
	CA            bool
	Now           time.Time
}

// LedgerPendingReading is the state of the ledger queue of a database: the
// number of transactions waiting for a block and how long the oldest has been
// waiting (as the monitor observed it, see PendingTracker).
type LedgerPendingReading struct {
	Database string
	Count    int64
	Age      time.Duration
}

// serverSubject is the subject of an alert about a server, optionally about
// one database or the CA of that server's check.
func serverSubject(host string, more ...string) string {
	return strings.Join(append([]string{"server:" + host}, more...), "|")
}

func (r Readings) certificateConditions() (conds []Condition) {
	for _, c := range r.Certificates {
		end := c.NotAfter.UTC().Format(time.RFC3339)
		if c.CA {
			left := c.NotAfter.Sub(c.Now)
			switch {
			case left <= CACritRemaining:
				conds = append(conds, Condition{"certificateExpiring", "critical", serverSubject(r.Host, "ca"),
					fmt.Sprintf("The certificate of the cluster CA (held by server %s) expires at %s. The CA is not rotated by this version of KingresDB.", r.Host, end)})
			case left <= CAWarnRemaining:
				conds = append(conds, Condition{"certificateExpiring", "warning", serverSubject(r.Host, "ca"),
					fmt.Sprintf("The certificate of the cluster CA (held by server %s) expires at %s. The CA is not rotated by this version of KingresDB.", r.Host, end)})
			}
			continue
		}
		// The renewal job renews when Remaining drops below RenewFraction; a
		// certificate that is below half of that has not been renewed.
		switch {
		case c.Remaining < c.RenewFraction/8 || !c.NotAfter.After(c.Now):
			conds = append(conds, Condition{"certificateExpiring", "critical", serverSubject(r.Host),
				fmt.Sprintf("The certificates of server %s expire at %s and have not been renewed.", r.Host, end)})
		case c.Remaining < c.RenewFraction/2:
			conds = append(conds, Condition{"certificateExpiring", "warning", serverSubject(r.Host),
				fmt.Sprintf("The certificates of server %s expire at %s and have not been renewed yet.", r.Host, end)})
		}
	}
	return conds
}

func (r Readings) ledgerConditions() (conds []Condition) {
	max := r.LedgerMaxAge
	if max == 0 {
		max = LedgerPendingMaxAge
	}
	for _, l := range r.LedgerPending {
		if l.Count == 0 || l.Age < max {
			continue
		}
		sev := "warning"
		if l.Age >= max*12 {
			sev = "critical"
		}
		conds = append(conds, Condition{"ledgerPendingTooOld", sev, serverSubject(r.Host, "database:"+l.Database),
			fmt.Sprintf("%d ledger transaction(s) of database %s on server %s have waited %s for their block.", l.Count, l.Database, r.Host, l.Age.Round(time.Second))})
	}
	return conds
}

// PendingTracker measures how long the oldest pending entry of a ledger queue
// has waited. The extension records no time for a pending entry, so the monitor
// remembers the lowest pending transaction number it saw and when it first saw
// it: while that entry stays the oldest, its age grows; when another entry is
// the oldest (the first one went into a block) the age starts again, which can
// only under-estimate; an empty queue is forgotten. The memory is lost at a
// restart of kingresd, which also restarts the count.
type PendingTracker struct {
	seen map[string]pendingState
}

type pendingState struct {
	oldest int64
	since  time.Time
}

// NewPendingTracker returns an empty tracker.
func NewPendingTracker() *PendingTracker { return &PendingTracker{seen: map[string]pendingState{}} }

// Observe records the queue of a database (the lowest pending transaction
// number, nil when the queue is empty, and its length) and returns the age of
// the oldest entry.
func (t *PendingTracker) Observe(db string, oldest *int64, count int64, now time.Time) time.Duration {
	if oldest == nil || count == 0 {
		delete(t.seen, db)
		return 0
	}
	st, ok := t.seen[db]
	if !ok || st.oldest != *oldest {
		t.seen[db] = pendingState{*oldest, now}
		return 0
	}
	return now.Sub(st.since)
}

// ServerVersions are the versions one server reports.
type ServerVersions struct {
	Server     string
	KingresDB  string
	PostgreSQL string
	Extensions map[string]string
}

// EvaluateVersions compares the versions of the servers (those that answered)
// and returns the cluster-wide condition versionMismatch when KingresDB, the
// PostgreSQL build or an extension has different versions on different servers.
// It needs at least two servers; the condition clears when they agree.
func EvaluateVersions(servers []ServerVersions) []Condition {
	if len(servers) < 2 {
		return nil
	}
	var parts []string
	describe := func(name string, get func(ServerVersions) string) {
		by := map[string][]string{}
		for _, s := range servers {
			v := get(s)
			if v == "" {
				v = "none"
			}
			by[v] = append(by[v], s.Server)
		}
		if len(by) < 2 {
			return
		}
		vs := make([]string, 0, len(by))
		for v := range by {
			vs = append(vs, v)
		}
		sort.Strings(vs)
		var items []string
		for _, v := range vs {
			ids := by[v]
			sort.Strings(ids)
			items = append(items, fmt.Sprintf("%s on %s", v, strings.Join(ids, ", ")))
		}
		items[0] = name + " " + items[0] // "KingresDB 0.2.0 on a, b; 0.3.0 on c"
		parts = append(parts, strings.Join(items, "; "))
	}
	describe("KingresDB", func(s ServerVersions) string { return s.KingresDB })
	describe("PostgreSQL", func(s ServerVersions) string { return s.PostgreSQL })
	exts := map[string]bool{}
	for _, s := range servers {
		for e := range s.Extensions {
			exts[e] = true
		}
	}
	names := make([]string, 0, len(exts))
	for e := range exts {
		names = append(names, e)
	}
	sort.Strings(names)
	for _, e := range names {
		describe(e, func(s ServerVersions) string { return s.Extensions[e] })
	}
	if len(parts) == 0 {
		return nil
	}
	return []Condition{{Kind: "versionMismatch", Severity: "warning", Subject: "cluster",
		Message: "The servers run different versions: " + strings.Join(parts, ". ") + "."}}
}
