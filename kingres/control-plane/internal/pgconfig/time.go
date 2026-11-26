// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package pgconfig

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/api"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/monitor"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/peerclient"
)

// Sample is one clock probe. W0 and W3 are the asking server's wall clock before sending and after
// the answer, M0 and M3 its monotonic clock at the same moments, Remote the other server's wall
// clock as it answered.
type Sample struct {
	W0, W3, Remote time.Time
	M0, M3         time.Duration
}

// Measurement is the estimate of the difference between two clocks.
type Measurement struct {
	// Offset is the other clock minus ours (positive: the other is ahead).
	Offset time.Duration
	// RoundTrip is measured with the monotonic clock, so a step of the wall clock during the probe
	// cannot disturb it.
	RoundTrip time.Duration
	// Uncertainty: the offset is exact only if both directions take equally long, so it can be wrong
	// by at most half the round trip; one millisecond is added for the resolution of the clocks.
	Uncertainty time.Duration
}

// Estimate computes the offset of one sample (the classical single-exchange estimate: the remote
// clock was read in the middle of the exchange).
func Estimate(s Sample) Measurement {
	rtt := s.M3 - s.M0
	if rtt < 0 {
		rtt = 0
	}
	mid := s.W0.Add(s.W3.Sub(s.W0) / 2)
	return Measurement{Offset: s.Remote.Sub(mid), RoundTrip: rtt, Uncertainty: rtt/2 + time.Millisecond}
}

// Best picks the sample with the smallest round trip: it has the least uncertainty.
func Best(samples []Sample) (Measurement, bool) {
	if len(samples) == 0 {
		return Measurement{}, false
	}
	best := Estimate(samples[0])
	for _, s := range samples[1:] {
		if m := Estimate(s); m.RoundTrip < best.RoundTrip {
			best = m
		}
	}
	return best, true
}

// Exceeds reports whether a clock difference is above the threshold once its uncertainty is taken
// off (so that a slow network alone never raises the alert), and by how many times.
func Exceeds(m Measurement, threshold time.Duration) (bool, float64) {
	d := m.Offset
	if d < 0 {
		d = -d
	}
	d -= m.Uncertainty
	if d <= threshold {
		return false, 0
	}
	return true, float64(d) / float64(threshold)
}

const probes = 5

type timeCache struct {
	mu sync.Mutex
	at time.Time
	v  api.ClusterTime
}

func ms(d time.Duration) float64 { return math.Round(float64(d)/float64(time.Millisecond)*1000) / 1000 }

func f64(v float64) *float64 { return &v }

// probe measures the clock of another server with n probes, 200 ms apart at most.
func (l *Leader) probe(ctx context.Context, server string, n int) (Measurement, TimeResponse, error) {
	var samples []Sample
	var last TimeResponse
	var lastErr error
	for i := 0; i < n; i++ {
		var t TimeResponse
		w0, m0 := l.Agent.now(), monotonic()
		c, cancel := context.WithTimeout(ctx, l.GatherTimeout)
		err := l.callServer(c, server, CallTime, nil, &t)
		cancel()
		m3, w3 := monotonic(), l.Agent.now()
		if err != nil {
			lastErr = err
			continue
		}
		last = t
		samples = append(samples, Sample{W0: w0, W3: w3, Remote: t.Now, M0: m0, M3: m3})
	}
	m, ok := Best(samples)
	if !ok {
		return Measurement{}, TimeResponse{}, lastErr
	}
	return m, last, nil
}

var monoStart = time.Now()

func monotonic() time.Duration { return time.Since(monoStart) }

// Time answers getClusterTime: the clocks of every server relative to this server's.
func (l *Leader) Time(ctx context.Context) (api.ClusterTime, error) {
	l.tm.mu.Lock()
	if time.Since(l.tm.at) < 5*time.Second && l.tm.v.Servers != nil {
		v := l.tm.v
		l.tm.mu.Unlock()
		return v, nil
	}
	l.tm.mu.Unlock()
	servers, err := l.st.servers(ctx)
	if err != nil {
		return api.ClusterTime{}, err
	}
	out := api.ClusterTime{MeasuredAt: time.Now().UTC(), ReferenceServerID: l.Self, MaxDifferenceMs: l.st.setting(ctx, "clock.maxDifferenceMs", 1000),
		Servers: make([]api.ServerTime, len(servers)), Databases: []api.DatabaseTimeZone{}}
	var wg sync.WaitGroup
	for i, s := range servers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			st := api.ServerTime{ServerID: s.ID}
			if s.ID == l.Self {
				t := l.Agent.Time(ctx)
				now := t.Now
				zero := 0.0
				st.Reachable, st.Clock, st.DifferenceMs, st.UncertaintyMs, st.RoundTripMs = true, &now, &zero, &zero, &zero
				fillZones(&st, t)
			} else if m, t, err := l.probe(ctx, s.ID, probes); err == nil {
				now := t.Now
				st.Reachable, st.Clock = true, &now
				st.DifferenceMs, st.UncertaintyMs, st.RoundTripMs = f64(ms(m.Offset)), f64(ms(m.Uncertainty)), f64(ms(m.RoundTrip))
				fillZones(&st, t)
			} else {
				l.Log.Debug("a server's clock could not be measured", "server", s.ID, "error", err)
			}
			out.Servers[i] = st
		}()
	}
	wg.Wait()
	if dbs, err := l.databaseZones(ctx); err == nil {
		out.Databases = dbs
	}
	l.tm.mu.Lock()
	l.tm.at, l.tm.v = time.Now(), out
	l.tm.mu.Unlock()
	return out, nil
}

func fillZones(st *api.ServerTime, t TimeResponse) {
	if t.OSTimeZone != "" {
		st.OSTimeZone = strp(t.OSTimeZone)
	}
	if t.TimeZone != "" {
		st.TimeZone = strp(t.TimeZone)
	}
	if t.LogTZ != "" {
		st.LogTimeZone = strp(t.LogTZ)
	}
}

// databaseZones lists the databases with the time zone set for each (the stored value).
func (l *Leader) databaseZones(ctx context.Context) ([]api.DatabaseTimeZone, error) {
	names, err := l.Agent.Databases(ctx)
	if err != nil {
		return nil, err
	}
	vals, err := l.st.values(ctx)
	if err != nil {
		return nil, err
	}
	zone := map[string]string{}
	for _, v := range vals {
		if v.Level == LevelDatabase && v.Name == "timezone" {
			zone[v.Scope] = v.Value
		}
	}
	sort.Strings(names)
	out := make([]api.DatabaseTimeZone, 0, len(names))
	for _, n := range names {
		d := api.DatabaseTimeZone{Name: n}
		if z, ok := zone[n]; ok {
			d.TimeZone = strp(z)
		}
		out = append(out, d)
	}
	return out, nil
}

// Databases lists the databases of this server's primary instance that a user can connect to.
func (a *Agent) Databases(ctx context.Context) ([]string, error) {
	c, err := a.conn(ctx, 1)
	if err != nil {
		return nil, err
	}
	defer func() { _ = c.Close(context.WithoutCancel(ctx)) }()
	rows, err := c.Query(ctx, `SELECT datname FROM pg_catalog.pg_database WHERE NOT datistemplate AND datallowconn AND datname <> 'kingres_system' ORDER BY 1`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// TimeZones lists the zone names PostgreSQL accepts.
func (a *Agent) TimeZones(ctx context.Context) ([]api.TimeZoneInfo, error) {
	c, err := a.conn(ctx, 1)
	if err != nil {
		return nil, err
	}
	defer func() { _ = c.Close(context.WithoutCancel(ctx)) }()
	rows, err := c.Query(ctx, `SELECT name, to_char(utc_offset, 'FMHH24:MI'), utc_offset < interval '0', is_dst FROM pg_catalog.pg_timezone_names
		WHERE name !~ '^(posix|right)/' ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []api.TimeZoneInfo{}
	for rows.Next() {
		var z api.TimeZoneInfo
		var hm string
		var neg bool
		if err := rows.Scan(&z.Name, &hm, &neg, &z.IsDaylightSaving); err != nil {
			return nil, err
		}
		hm = strings.TrimPrefix(hm, "-")
		if len(hm) < 5 {
			hm = strings.Repeat("0", 5-len(hm)) + hm
		}
		sign := "+"
		if neg {
			sign = "-"
		}
		z.UTCOffset = sign + hm
		out = append(out, z)
	}
	return out, rows.Err()
}

// CheckClocks measures the clock of every other server and raises clockDifference for the one that is
// off by more than the threshold (less the uncertainty of the measurement), clears it when the server
// is back within it. It is the manager leader's job; the alert is about the other server, so it is
// reconciled in the scope of that server and not by the monitor round of this one.
func (l *Leader) CheckClocks(ctx context.Context) {
	if l.IsLeader == nil || !l.IsLeader(ctx) {
		return
	}
	servers, err := l.st.servers(ctx)
	if err != nil {
		return
	}
	threshold := time.Duration(l.st.setting(ctx, "clock.maxDifferenceMs", 1000)) * time.Millisecond
	for _, s := range servers {
		if s.ID == l.Self {
			continue
		}
		m, _, err := l.probe(ctx, s.ID, 3)
		if err != nil {
			if !errors.Is(err, peerclient.ErrUnreachable) {
				l.Log.Debug("clock probe failed", "server", s.ID, "error", err)
			}
			continue // a server that cannot be reached is the business of serverDown, and its alert stays as it is
		}
		var conds []monitor.Condition
		if over, times := Exceeds(m, threshold); over {
			sev := "warning"
			if times >= 10 {
				sev = "critical"
			}
			conds = append(conds, monitor.Condition{Kind: "clockDifference", Severity: sev, Subject: "server:" + s.ID,
				Message: fmt.Sprintf("The clock of server %s differs from the manager leader's (%s) by %s (+/- %s); the limit is %s. kingresd does not set the system clock: check time synchronization (chrony or NTP) on that server.",
					s.ID, l.Self, m.Offset.Round(time.Millisecond), m.Uncertainty.Round(time.Millisecond), threshold)})
		}
		if err := l.Sys(ctx, func(c *pgx.Conn) error {
			return monitor.ReconcileScope(ctx, c, "server:"+s.ID, conds, map[string]bool{"clockDifference": true}, time.Now())
		}); err != nil {
			l.Log.Warn("updating the clock alert failed", "server", s.ID, "error", err)
		}
	}
}
