// SPDX-License-Identifier: AGPL-3.0-only

package standby

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/audit"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/clock"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/cluster"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/ha"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/instance"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/pki"
)

// Agent is the part of the standby machinery that runs on every server: it
// creates, removes and rebuilds the standby instances of this server on the
// leader's request, keeps them running and their access files current, and, for
// the group whose primary this server runs, maintains the slots and
// synchronous_standby_names by the rule of the design without asking the leader
// (the leader may be unreachable exactly when a standby is lost).
type Agent struct {
	Svc     *cluster.Service
	Self    string
	DataDir string
	Log     *slog.Logger
	// Clock is the monotonic clock of the grace period (default: the process clock).
	Clock clock.Clock
	// Audit records a state-changing operation in the audit log of this server.
	Audit func(ctx context.Context, ev audit.Event)
	// Frozen reports that a failover of the group is being decided (nil: never); a freeze
	// asked for by the internal call (Freeze) counts as well.
	Frozen func(group int32) bool
	// NameMatches asks the extension whether a name matches a streaming standby of this
	// server's primary (kingres.standby_name_matches); nil: ask the database.
	NameMatches func(ctx context.Context, dir int, name string) (bool, error)
	// Grace, SlotWarn and SlotLimit are the settings standby-sync-grace,
	// slot-retention-warn-bytes and slot-retention-limit-bytes.
	Grace     time.Duration
	SlotWarn  int64
	SlotLimit int64
	// PortBase is the first port tried for a standby instance (0: pg-port plus 1000).
	PortBase int
	// Tick is the period of the supervision loop (default 3 s).
	Tick time.Duration

	local *localState

	opMu sync.Mutex // one create, remove or rebuild at a time

	mu        sync.Mutex
	syncs     map[int32]*SyncState
	primaries map[int32]*PrimaryStatus
	// frozen and gone are the freeze of a group and the servers whose standbys are declared
	// gone, each with the monotonic time it lapses at (freeze.go).
	frozen   map[int32]time.Duration
	frozenBy map[int32]freezeOwner
	gone     map[string]time.Duration
	rounds   int64
	slotWarn map[int32]map[int64]int64
	access   map[string]fileStamp
	kick     chan struct{}
}

type fileStamp struct {
	mod  time.Time
	size int64
}

// NewAgent opens the local state.
func NewAgent(a *Agent) (*Agent, error) {
	if a.Clock == nil {
		a.Clock = clock.NewReal()
	}
	if a.Log == nil {
		a.Log = slog.New(slog.DiscardHandler)
	}
	if a.Tick == 0 {
		a.Tick = 3 * time.Second
	}
	if a.Grace == 0 {
		a.Grace = 30 * time.Second
	}
	l, err := openLocalState(a.DataDir)
	if err != nil {
		return nil, err
	}
	a.local = l
	a.syncs = map[int32]*SyncState{}
	a.slotWarn = map[int32]map[int64]int64{}
	a.primaries = map[int32]*PrimaryStatus{}
	a.frozen = map[int32]time.Duration{}
	a.frozenBy = map[int32]freezeOwner{}
	a.gone = map[string]time.Duration{}
	a.loadFreezes()
	a.access = map[string]fileStamp{}
	a.kick = make(chan struct{}, 1)
	return a, nil
}

// List implements ha.Standbys: the standbys of the cluster as this server was last
// told. State is "running" for a standby that exists and "starting" for one that
// is being created; what the primary sees of it (streaming, lag) is read from the
// primary by the replication view.
func (a *Agent) List(context.Context) ([]ha.Standby, error) {
	st := a.local.snapshot()
	out := make([]ha.Standby, 0, len(st.Standbys))
	for _, e := range st.Standbys {
		s := ha.Standby{ID: e.ID, Group: e.Group, ServerID: e.Server, Port: e.Port, CatalogNode: e.CatalogNode, State: "starting"}
		if e.Ready {
			s.State = "running"
		}
		out = append(out, s)
	}
	return out, nil
}

// HasStandby implements ha.Standbys from the local copy of the list.
func (a *Agent) HasStandby(group int32) bool {
	return len(a.local.snapshot().entriesOfGroup(group)) > 0
}

func (a *Agent) mgr() *instance.Manager { return a.Svc.Manager() }

func (a *Agent) audit(ctx context.Context, action, resource, outcome string, details map[string]any) {
	if a.Audit == nil {
		return
	}
	a.Audit(ctx, audit.Event{Actor: "system", Source: "system", Action: action, ResourceType: "standby", ResourceID: resource, Outcome: outcome, Details: details})
}

// Plan takes the list the leader pushed and applies it to this server's primary
// at once. An older list than the one held is ignored.
func (a *Agent) Plan(ctx context.Context, p PlanRequest) error {
	if len(p.Standbys) > 4096 {
		return errors.New("standby: the plan is too large")
	}
	for _, e := range p.Standbys {
		if err := validEntry(e); err != nil {
			return err
		}
	}
	if _, err := a.local.replace(p); err != nil {
		return fmt.Errorf("standby: storing the plan: %w", err)
	}
	select {
	case a.kick <- struct{}{}:
	default:
	}
	return nil
}

func validEntry(e Entry) error {
	if e.ID < 1 || e.Group < 0 || e.Group > 1<<16 || e.Server == "" || e.PrimaryServer == "" || len(e.Server) > 255 || len(e.PrimaryServer) > 255 ||
		e.PrimaryPort < 1 || e.PrimaryPort > 65535 || e.Port < 0 || e.Port > 65535 || e.InstanceID < 0 || e.InstanceID > 1<<20 {
		return errors.New("standby: the entry is not valid")
	}
	return nil
}

// Run is the supervision loop: it resumes the standbys of this server (a server
// restart starts them again) and then, every Tick or when a plan arrives,
// supervises them and the primary's side of the group.
func (a *Agent) Run(ctx context.Context) {
	t := time.NewTicker(a.Tick)
	defer t.Stop()
	for ctx.Err() == nil {
		a.once(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-a.kick:
		}
	}
}

// once is one round of supervision.
func (a *Agent) once(ctx context.Context) {
	if !a.Svc.Enabled() || !a.mgr().Exists(cluster.NodeID) {
		return
	}
	rctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	a.superviseStandbys(rctx)
	if err := a.maintainPrimaries(rctx); err != nil && ctx.Err() == nil {
		a.Log.Debug("standby: maintaining the primary side", "error", err)
	}
}

// metaOf reads the file that says which standby a directory holds.
func (a *Agent) metaOf(id int) (Entry, error) {
	dir, err := a.mgr().Dir(id)
	if err != nil {
		return Entry{}, err
	}
	b, err := os.ReadFile(filepath.Join(dir, metaFile)) //nolint:gosec // fixed name below the manager root
	if err != nil {
		return Entry{}, err
	}
	var e Entry
	if err := json.Unmarshal(b, &e); err != nil {
		return Entry{}, fmt.Errorf("standby: %s: %w", metaFile, err)
	}
	return e, nil
}

func (a *Agent) writeMeta(id int, e Entry) error {
	dir, err := a.mgr().Dir(id)
	if err != nil {
		return err
	}
	b, _ := json.Marshal(e)
	return pki.WriteFile(filepath.Join(dir, metaFile), b, 0o600)
}

// localStandby is a standby instance of this server with the directory that holds it.
type localStandby struct {
	Dir int
	Entry
}

// localStandbys lists the standby instances of this server that are complete: the
// directories whose role file says standby and that carry the file naming the standby.
func (a *Agent) localStandbys() []localStandby {
	var out []localStandby
	for _, id := range a.mgr().IDs() {
		if !a.mgr().MustRole(id).IsStandby() { // a primary, and an instance whose role file is broken (FO-09)
			continue
		}
		if e, err := a.metaOf(id); err == nil {
			out = append(out, localStandby{Dir: id, Entry: e})
		}
	}
	return out
}

// freePort finds a port for a new standby instance: the first free one from PortBase on
// (default: the server's pg-port plus 1000, away from the ports of the other servers of a one-machine cluster)
// that no other instance of this server uses.
func (a *Agent) freePort() (int, error) {
	used := map[int]bool{a.Svc.PGPort(): true}
	for _, id := range a.mgr().IDs() {
		if mk, err := a.mgr().Managed(id); err == nil {
			used[mk.Port] = true
		}
	}
	base := a.PortBase
	if base == 0 {
		base = a.Svc.PGPort() + 1000
	}
	for p := base; p < base+500 && p < 65535; p++ {
		if used[p] {
			continue
		}
		l, err := net.Listen("tcp", ":"+strconv.Itoa(p))
		if err != nil {
			continue
		}
		_ = l.Close()
		return p, nil
	}
	return 0, errors.New("standby: no free port for the standby instance")
}

// localPrimary is a primary instance this server runs and the shard group it serves.
type localPrimary struct {
	Dir   int
	Group int32
}

// localPrimaries are the primary instances of this server with their groups, read
// from the role files every round (after a failover a server runs a primary that is
// not instance 1, or none). An instance whose group is not known yet is skipped until
// the catalog has named it (cluster.Service.SyncRoles).
func (a *Agent) localPrimaries() []localPrimary {
	var out []localPrimary
	for _, li := range a.Svc.WritableInstances() {
		if li.Role.Group >= 0 {
			out = append(out, localPrimary{Dir: li.ID, Group: li.Role.Group})
		}
	}
	return out
}

// sortedIDs is a helper for stable output.
func sortedIDs(m map[int64]bool) []int64 {
	out := make([]int64, 0, len(m))
	for id := range m {
		out = append(out, id)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func (a *Agent) primaryGroups() []int32 {
	out := make([]int32, 0, len(a.primaries))
	for g := range a.primaries {
		out = append(out, g)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}
