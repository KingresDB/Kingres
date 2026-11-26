// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package failover

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"
)

// Automatic failover (design ha-phase5-part2.md, section 4). One rule, two loops: Auto runs on the manager leader and looks at
// every other server; the candidate monitor of the map group (cmd/kingresd/mapmonitor.go) runs the same evidence for the case in which the
// leader is the server that is gone. The rule is in Evaluate, a function of what the loop knows, so that every row of the table of
// section 4.3 has a test.

// AutoSettings are the settings of the contract (autoFailover.*) and what the evidence needs of the cluster.
type AutoSettings struct {
	Enabled bool
	// TimeoutSeconds is the setting, MaxCount the limit and EventsSinceReset the counter.
	TimeoutSeconds   int
	MaxCount         int
	EventsSinceReset int
	// FailureTimeout is the failure timeout of the heartbeats: the dead timeout never goes below three times it (4.2).
	FailureTimeout time.Duration
}

// EffectiveTimeout is max(timeoutSeconds, 3 x failure-timeout): a cluster with a long failure timeout is not failed over faster than it
// can tell a failure from a pause, and the old lease has run out before an automatic task starts.
func (s AutoSettings) EffectiveTimeout() time.Duration {
	t := time.Duration(s.TimeoutSeconds) * time.Second
	if f := 3 * s.FailureTimeout; f > t {
		return f
	}
	return t
}

// AutoServer is a server as the loop sees it.
type AutoServer struct {
	ID string
	// Dead says that the tracker of this server calls the server dead, Silence how long it has been silent (monotonic).
	Dead    bool
	Silence time.Duration
	// NotLiveFor counts the members (this server included) that report the server as not live; Members is the number of members and
	// Answered how many answered the question.
	NotLiveFor, Answered, Members int
	// Groups are the groups whose primary the server runs, and WithStandby those of them that have a ready standby.
	Groups, WithStandby []int32
}

// AutoView is everything one evaluation reads.
type AutoView struct {
	Settings AutoSettings
	Servers  []AutoServer
	// HearsMajority says that this server hears a strict majority of the members.
	HearsMajority bool
	// Unfinished says that a task of the failover class is unfinished (running, pending or paused for an operator).
	Unfinished bool
	// LastEnded is when a failover of the group ended (the cool-down of 5 minutes).
	LastEnded map[int32]time.Time
	Now       time.Time
}

// AutoCooldown is the fixed cool-down against ping-pong (a manual failover is not subject to it).
const AutoCooldown = 5 * time.Minute

// AutoDecision is what one evaluation says.
type AutoDecision struct {
	// Start is the server to fail over now ("" none).
	Start string
	// Reason is why nothing starts, or what starts, for the log.
	Reason string
	// LimitReached asks for the alert autoFailoverLimitReached (and its clearing when false and Settings.Enabled).
	LimitReached bool
	// Blocked lists groups that need a standby and have none, for failoverBlocked.
	Blocked []int32
}

// Evaluate is the rule of section 4.2 and 4.3.
func Evaluate(v AutoView) AutoDecision {
	s := v.Settings
	d := AutoDecision{}
	if !s.Enabled {
		d.Reason = "automatic failover is off"
		return d
	}
	if s.MaxCount > 0 && s.EventsSinceReset >= s.MaxCount {
		d.LimitReached, d.Reason = true, fmt.Sprintf("the limit of %d automatic failovers is reached", s.MaxCount)
		return d
	}
	if !v.HearsMajority {
		d.Reason = "this server does not hear a majority of the servers"
		return d
	}
	timeout := s.EffectiveTimeout()
	servers := append([]AutoServer(nil), v.Servers...)
	sort.Slice(servers, func(i, j int) bool { return servers[i].ID < servers[j].ID })
	for _, sv := range servers {
		if !sv.Dead || sv.Silence < timeout {
			continue
		}
		// A server is a candidate only when a majority of the members agree that it is not live.
		if sv.Members == 0 || sv.NotLiveFor*2 <= sv.Members {
			d.Reason = fmt.Sprintf("server %s is silent for this server only (%d of %d members agree)", sv.ID, sv.NotLiveFor, sv.Members)
			continue
		}
		if len(sv.WithStandby) == 0 {
			for _, g := range sv.Groups {
				d.Blocked = append(d.Blocked, g)
			}
			d.Reason = fmt.Sprintf("server %s has no group with a standby to promote", sv.ID)
			continue
		}
		if len(sv.WithStandby) < len(sv.Groups) {
			for _, g := range sv.Groups {
				has := false
				for _, w := range sv.WithStandby {
					has = has || w == g
				}
				if !has {
					d.Blocked = append(d.Blocked, g)
				}
			}
		}
		if v.Unfinished {
			d.Reason = "another failover is unfinished"
			return d
		}
		cooling := false
		for _, g := range sv.WithStandby {
			if end, ok := v.LastEnded[g]; ok && v.Now.Sub(end) < AutoCooldown {
				cooling = true
			}
		}
		if cooling {
			d.Reason = fmt.Sprintf("a failover of a group of server %s ended less than %s ago", sv.ID, AutoCooldown)
			continue
		}
		d.Start, d.Reason = sv.ID, fmt.Sprintf("server %s has been silent for %s and %d of %d members agree", sv.ID, sv.Silence.Round(time.Second), sv.NotLiveFor, sv.Members)
		return d
	}
	return d
}

// ErrAutoNotLeader is what View answers on a server that is not the manager leader: the loop then does nothing.
var ErrAutoNotLeader = errors.New("failover: this server is not the manager leader")

// AutoPorts is what the loop reads and does.
type AutoPorts interface {
	// View reads everything of one evaluation (it returns an error when the cluster cannot be read now).
	View(ctx context.Context) (AutoView, error)
	// StopRebalance stops a rebalance that excludes the failover and waits for it (design 4.5): the caller does not start a task before it
	// returns true.
	StopRebalance(ctx context.Context, server string) (bool, error)
	// Start starts the task of the failover of a server with trigger automatic.
	Start(ctx context.Context, server string) error
	// Alert raises or clears an alert of the failover.
	Alert(ctx context.Context, kind, severity, subject, message string, raise bool)
	// Log is for the reasons.
	Log(msg string, kv ...any)
}

// Auto is the loop on the manager leader.
type Auto struct {
	P        AutoPorts
	Interval time.Duration // default 5 s
	Backoff  time.Duration // after a failed start, default 60 s (doubles up to 10 min)
	Sleep    func(ctx context.Context, d time.Duration) error
	Now      func() time.Time

	mu      sync.Mutex
	until   map[string]time.Time
	back    map[string]time.Duration
	blocked map[int32]bool
	limited bool
	// limitKnown says that this loop has told the alert about the limit at least once: a leader that took over does not know what its predecessor
	// raised, so its first look raises or clears the alert, which is idempotent (F4-13).
	limitKnown bool
}

func (a *Auto) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

// Step is one evaluation and, when the rule says so, one start. It returns what it did, for the tests.
func (a *Auto) Step(ctx context.Context) (AutoDecision, error) {
	v, err := a.P.View(ctx)
	if err != nil {
		if errors.Is(err, ErrAutoNotLeader) {
			return AutoDecision{Reason: "not the leader"}, nil
		}
		return AutoDecision{}, err
	}
	v.Now = a.now()
	d := Evaluate(v)
	a.mu.Lock()
	if a.until == nil {
		a.until, a.back, a.blocked = map[string]time.Time{}, map[string]time.Duration{}, map[int32]bool{}
	}
	if !a.limitKnown || d.LimitReached != a.limited {
		a.limited, a.limitKnown = d.LimitReached, true
		a.mu.Unlock()
		a.P.Alert(ctx, "autoFailoverLimitReached", "critical", "cluster", "The automatic failover limit is reached; fail over by hand or reset the counter.", d.LimitReached)
		a.mu.Lock()
	}
	cur := map[int32]bool{}
	for _, g := range d.Blocked {
		cur[g] = true
	}
	var raise, clear []int32
	for g := range cur {
		if !a.blocked[g] {
			raise = append(raise, g)
		}
	}
	for g := range a.blocked {
		if !cur[g] {
			clear = append(clear, g)
		}
	}
	a.blocked = cur
	if d.Start != "" && a.now().Before(a.until[d.Start]) {
		d.Start, d.Reason = "", "backing off after a failed start"
	}
	a.mu.Unlock()
	for _, g := range raise {
		a.P.Alert(ctx, "failoverBlocked", "warning", fmt.Sprintf("group:%d", g), fmt.Sprintf("Group %d has no standby to promote.", g), true)
	}
	for _, g := range clear {
		a.P.Alert(ctx, "failoverBlocked", "warning", fmt.Sprintf("group:%d", g), "", false)
	}
	if d.Start == "" {
		return d, nil
	}
	a.P.Log("automatic failover", "server", d.Start, "reason", d.Reason)
	if stopped, err := a.P.StopRebalance(ctx, d.Start); err != nil || !stopped {
		d.Start, d.Reason = "", "a rebalance that excludes the failover is still stopping"
		return d, err
	}
	if err := a.P.Start(ctx, d.Start); err != nil {
		server := d.Start
		a.mu.Lock()
		b := a.back[server]
		if b == 0 {
			b = orDur(a.Backoff, time.Minute)
		} else if b *= 2; b > 10*time.Minute {
			b = 10 * time.Minute
		}
		a.back[server] = b
		a.until[server] = a.now().Add(b)
		a.mu.Unlock()
		d.Start, d.Reason = "", "the start failed: "+err.Error()
		return d, nil
	}
	return d, nil
}

// Run loops until ctx ends.
func (a *Auto) Run(ctx context.Context) {
	for ctx.Err() == nil {
		if _, err := a.Step(ctx); err != nil && ctx.Err() == nil {
			a.P.Log("automatic failover: the cluster cannot be read", "error", err.Error())
		}
		d := orDur(a.Interval, 5*time.Second)
		if a.Sleep != nil {
			if a.Sleep(ctx, d) != nil {
				return
			}
			continue
		}
		select {
		case <-ctx.Done():
		case <-time.After(d):
		}
	}
}
