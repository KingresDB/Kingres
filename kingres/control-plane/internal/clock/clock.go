// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

// Package clock separates the two kinds of time the high-availability code uses.
// Everything that decides something (a heartbeat is late, a lease has expired)
// uses Mono, a monotonic duration that no step of the system clock can change;
// Wall is only for display and for audit records. Only durations ever travel
// between servers, never a wall-clock time.
//
// Tests inject a Fake. The fault hook of the tests (package fault) can step
// the wall clock of a running kingresd (Stepped); a decision that changes when
// it does has used the wrong clock.
package clock

import (
	"sync"
	"time"
)

// Clock is the time source of the high-availability features.
type Clock interface {
	// Mono is the monotonic time since an arbitrary start; it never goes
	// backwards and is not affected by a step of the system clock.
	Mono() time.Duration
	// Wall is the wall-clock time, for display and audit only.
	Wall() time.Time
}

// Real is the process clock. Its Mono is read from Go's monotonic clock.
type Real struct{ start time.Time }

// NewReal returns the process clock.
func NewReal() *Real { return &Real{start: time.Now()} }

// Mono implements Clock.
func (r *Real) Mono() time.Duration { return time.Since(r.start) }

// Wall implements Clock.
func (r *Real) Wall() time.Time { return time.Now() }

// Stepped is a clock whose wall time is shifted by Offset() (the fault hook's
// clock step); Mono is untouched, as a real step of the system clock leaves the
// monotonic clock alone.
type Stepped struct {
	Base   Clock
	Offset func() time.Duration
}

// Mono implements Clock.
func (s Stepped) Mono() time.Duration { return s.Base.Mono() }

// Wall implements Clock.
func (s Stepped) Wall() time.Time { return s.Base.Wall().Round(0).Add(s.Offset()) }

// Fake is a manually advanced clock for unit tests. Advance moves both readings.
type Fake struct {
	mu   sync.Mutex
	mono time.Duration
	wall time.Time
}

// NewFake starts a fake clock at the given wall time and monotonic zero.
func NewFake(wall time.Time) *Fake { return &Fake{wall: wall} }

// Mono implements Clock.
func (f *Fake) Mono() time.Duration { f.mu.Lock(); defer f.mu.Unlock(); return f.mono }

// Wall implements Clock.
func (f *Fake) Wall() time.Time { f.mu.Lock(); defer f.mu.Unlock(); return f.wall }

// Advance moves time forward by d on both readings.
func (f *Fake) Advance(d time.Duration) {
	f.mu.Lock()
	f.mono += d
	f.wall = f.wall.Add(d)
	f.mu.Unlock()
}

// StepWall moves only the wall time (a step of the system clock).
func (f *Fake) StepWall(d time.Duration) { f.mu.Lock(); f.wall = f.wall.Add(d); f.mu.Unlock() }
