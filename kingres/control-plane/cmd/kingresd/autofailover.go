// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/failover"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/leader"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/rebalance"
)

// autoPorts reads the cluster for the loop of the automatic failover on the manager leader (failover.Auto, design section 4).
type autoPorts struct {
	mf             *mapFailover
	fos            *failover.Service
	rbs            *rebalance.Service
	leaders        *leader.Resolver
	self           string
	failureTimeout time.Duration
	log            *slog.Logger
}

// newAutoFailover builds the loop. It does nothing on a server that is not the leader.
func newAutoFailover(mf *mapFailover, fos *failover.Service, rbs *rebalance.Service, leaders *leader.Resolver, self string, failureTimeout time.Duration, log *slog.Logger) *failover.Auto {
	return &failover.Auto{P: &autoPorts{mf: mf, fos: fos, rbs: rbs, leaders: leaders, self: self, failureTimeout: failureTimeout, log: log}}
}

func (p *autoPorts) Log(msg string, kv ...any) { p.log.Info(msg, kv...) }

func (p *autoPorts) Alert(ctx context.Context, kind, severity, subject, message string, raise bool) {
	p.mf.pl.Alert(ctx, kind, severity, subject, message, raise)
}

// autoSettings reads the settings of the automatic failover (and the counter) from a system database connection.
func autoSettings(ctx context.Context, c *pgx.Conn, failureTimeout time.Duration) (failover.AutoSettings, error) {
	s := failover.AutoSettings{TimeoutSeconds: 120, MaxCount: 1, FailureTimeout: failureTimeout}
	rows, err := c.Query(ctx, `SELECT key, value FROM kingres_cp.cluster_setting WHERE key LIKE 'autoFailover.%'`)
	if err != nil {
		return s, err
	}
	defer rows.Close()
	for rows.Next() {
		var k string
		var v []byte
		if err := rows.Scan(&k, &v); err != nil {
			return s, err
		}
		var n int
		var b bool
		switch k {
		case "autoFailover.enabled":
			if json.Unmarshal(v, &b) == nil {
				s.Enabled = b
			}
		case "autoFailover.timeoutSeconds":
			if json.Unmarshal(v, &n) == nil {
				s.TimeoutSeconds = n
			}
		case "autoFailover.maxCount":
			if json.Unmarshal(v, &n) == nil {
				s.MaxCount = n
			}
		case "autoFailover.eventsSinceReset":
			if json.Unmarshal(v, &n) == nil {
				s.EventsSinceReset = n
			}
		}
	}
	return s, rows.Err()
}

// corroborate counts the members (this server included) that report a server as not live (GET /liveness) and how many answered.
func corroborate(ctx context.Context, mf *mapFailover, members []string, server string) (notLive, answered int) {
	for _, m := range members {
		if m == server {
			continue
		}
		if m == mf.self {
			answered++
			if mf.pl.Live(ctx, server) != "live" {
				notLive++
			}
			continue
		}
		cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		var ans LivenessAnswer
		err := mf.pl.client.Do(cctx, m, "GET", "/liveness", nil, &ans)
		cancel()
		if err != nil {
			continue
		}
		answered++
		for _, lp := range ans.Peers {
			if lp.Server == server && lp.State != "live" {
				notLive++
			}
		}
	}
	return notLive, answered
}

func (p *autoPorts) View(ctx context.Context) (failover.AutoView, error) {
	var v failover.AutoView
	id, err := p.leaders.Get(ctx)
	if err != nil || id != p.self {
		return v, failover.ErrAutoNotLeader
	}
	pl := p.mf.pl
	err = pl.sys(ctx, func(c *pgx.Conn) error {
		var e error
		v.Settings, e = autoSettings(ctx, c, p.failureTimeout)
		return e
	})
	if err != nil {
		return v, err
	}
	if !v.Settings.Enabled {
		return v, nil // nothing else is read while it is off
	}
	nodes, err := p.mf.cat.Nodes(ctx, failover.SystemDB)
	if err != nil {
		return v, err
	}
	members, err := pl.Members(ctx)
	if err != nil {
		return v, err
	}
	live := 0
	for _, m := range members {
		if m == p.self || pl.Live(ctx, m) == "live" {
			live++
		}
	}
	v.HearsMajority = live*2 > len(members)
	byServer := map[string]*failover.AutoServer{}
	for _, n := range nodes {
		if n.Role != "primary" || n.Host == p.self {
			continue
		}
		sv := byServer[n.Host]
		if sv == nil {
			sv = &failover.AutoServer{ID: n.Host, Members: len(members)}
			byServer[n.Host] = sv
		}
		sv.Groups = append(sv.Groups, n.Group)
		if recs, err := pl.Standbys(ctx, n.Group); err == nil {
			for _, r := range recs {
				if r.State == "ready" {
					sv.WithStandby = append(sv.WithStandby, n.Group)
					break
				}
			}
		}
	}
	timeout := v.Settings.EffectiveTimeout()
	hosts := make([]string, 0, len(byServer))
	for h := range byServer {
		hosts = append(hosts, h)
	}
	sort.Strings(hosts)
	for _, h := range hosts {
		sv := byServer[h]
		if silence, ok := p.mf.pl.reg.Liveness().Silence(h); ok {
			sv.Silence = silence
		}
		sv.Dead = pl.Live(ctx, h) == "dead"
		if sv.Dead && sv.Silence >= timeout {
			sv.NotLiveFor, sv.Answered = corroborate(ctx, p.mf, members, h)
		}
		v.Servers = append(v.Servers, *sv)
	}
	if v.Unfinished, err = p.fos.Unfinished(ctx); err != nil {
		return v, err
	}
	if v.LastEnded, err = p.fos.LastEnded(ctx); err != nil {
		return v, err
	}
	return v, nil
}

func (p *autoPorts) StopRebalance(ctx context.Context, server string) (bool, error) {
	if p.rbs == nil {
		return true, nil
	}
	if _, err := p.rbs.StopForFailover(ctx, server); err != nil {
		return false, err
	}
	// A move that is switching is not interrupted: the wait is long, the loop asks again.
	return p.rbs.WaitStopped(ctx, 60*time.Second)
}

func (p *autoPorts) Start(ctx context.Context, server string) error {
	t, err := p.fos.StartAutomatic(ctx, server)
	if err != nil {
		return err
	}
	p.log.Info("automatic failover started", "server", server, "task", t.ID)
	p.mf.pl.Audit(ctx, "autoFailover", 0, map[string]any{"server": server, "task": t.ID})
	return nil
}
