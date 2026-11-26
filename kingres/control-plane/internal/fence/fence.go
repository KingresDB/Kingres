// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

// Package fence carries the fencing epoch of a task run to the servers its steps
// call, and lets those servers refuse a call of a run that lost its lease.
//
// The task engine's lease epoch (task.Env.Fence) grows with every claim of a task.
// A step that was running on a leader that was then paused or cut off, and whose
// lease another run took over, may wake up and send its next call. The store
// already refuses its writes; this package makes the servers it calls refuse too:
// the engine puts a Token in the step's context, the peer client sends it in the
// header X-Kingres-Fence, and the callee's Registry remembers the highest epoch it
// has seen for each task and refuses a lower one.
//
// The rule is deliberately small: an epoch equal to or above the highest seen is
// accepted (a run makes many calls), a lower one is refused, and a callee never
// lowers what it remembers. What it cannot do is refuse a late call when the newer
// run has not yet called that callee; the store's epoch check covers that case for
// everything a step writes in the system database.
package fence

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// Header is the request header that carries a Token.
const Header = "X-Kingres-Fence"

// ErrFenced means the call comes from a run whose epoch is lower than one the callee has seen.
var ErrFenced = errors.New("fence: the task run was superseded by a newer one")

// Token names a run of a task: the task id and the epoch of its lease.
type Token struct {
	Task  int64
	Epoch int64
}

// String is the header value, "<task id>:<epoch>".
func (t Token) String() string {
	return strconv.FormatInt(t.Task, 10) + ":" + strconv.FormatInt(t.Epoch, 10)
}

// Parse reads a header value.
func Parse(v string) (Token, error) {
	a, b, ok := strings.Cut(v, ":")
	if !ok {
		return Token{}, errors.New("fence: malformed token")
	}
	task, err1 := strconv.ParseInt(a, 10, 64)
	epoch, err2 := strconv.ParseInt(b, 10, 64)
	if err1 != nil || err2 != nil || task < 1 || epoch < 0 {
		return Token{}, errors.New("fence: malformed token")
	}
	return Token{Task: task, Epoch: epoch}, nil
}

type ctxKey struct{}

// With returns a context that carries the token; calls made with it are fenced.
func With(ctx context.Context, t Token) context.Context { return context.WithValue(ctx, ctxKey{}, t) }

// From returns the token of a context.
func From(ctx context.Context) (Token, bool) {
	t, ok := ctx.Value(ctxKey{}).(Token)
	return t, ok
}

// maxEntries bounds the registry: a task older than the newest maxEntries tasks
// seen cannot still be running, so its entry is dropped.
const maxEntries = 4096

// Registry is the callee's memory of the highest epoch seen per task. It is safe
// for concurrent use. With a directory it is kept across restarts: a callee that
// restarts must not let a superseded run through just because it forgot.
type Registry struct {
	path string

	mu   sync.Mutex
	seen map[int64]int64
}

// NewRegistry returns an in-memory registry.
func NewRegistry() *Registry { return &Registry{seen: map[int64]int64{}} }

// Open returns a registry kept in dir/fence.json. A missing file is an empty
// registry; a damaged one is too (the error is returned together with a usable
// registry, so that the caller can log it): refusing every fenced call because a
// file is damaged would stop the cluster's tasks, and the epoch checks of the store remain.
func Open(dir string) (*Registry, error) {
	r := NewRegistry()
	r.path = filepath.Join(dir, "fence.json")
	b, err := os.ReadFile(r.path)
	if errors.Is(err, os.ErrNotExist) {
		return r, nil
	}
	if err == nil {
		var m map[string]int64
		if err = json.Unmarshal(b, &m); err == nil {
			for k, v := range m {
				if id, perr := strconv.ParseInt(k, 10, 64); perr == nil {
					r.seen[id] = v
				}
			}
			return r, nil
		}
	}
	return r, fmt.Errorf("reading the fence memory (starting empty): %w", err)
}

// Check accepts or refuses a call. It records a higher epoch (on disk first, so
// that a crash right after cannot lose it) before it accepts the call.
func (r *Registry) Check(t Token) error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	cur, known := r.seen[t.Task]
	if known && t.Epoch < cur {
		return ErrFenced
	}
	if known && t.Epoch == cur {
		return nil
	}
	r.seen[t.Task] = t.Epoch
	r.prune()
	// A disk that cannot be written must not stop tasks: the memory of the process
	// still refuses late calls until the next restart.
	_ = r.save()
	return nil
}

func (r *Registry) prune() {
	if len(r.seen) <= maxEntries {
		return
	}
	ids := make([]int64, 0, len(r.seen))
	for id := range r.seen {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	for _, id := range ids[:len(ids)-maxEntries] {
		delete(r.seen, id)
	}
}

func (r *Registry) save() error {
	if r.path == "" {
		return nil
	}
	m := make(map[string]int64, len(r.seen))
	for id, e := range r.seen {
		m[strconv.FormatInt(id, 10)] = e
	}
	b, _ := json.Marshal(m)
	tmp := r.path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600) //nolint:gosec // a file of our own data directory
	if err != nil {
		return err
	}
	if _, err = f.Write(b); err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, r.path)
}
