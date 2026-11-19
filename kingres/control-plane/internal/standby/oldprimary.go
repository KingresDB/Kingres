// SPDX-License-Identifier: AGPL-3.0-only

package standby

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"strconv"
	"strings"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/audit"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/instance"
)

// The old primary comes back as a standby (design ha-phase5-part2.md, section 6; increment 4).
//
// A directory whose role file says primary is rewound or copied over only when this server can prove, from what only
// it knows, that it is the former primary of the group and that nothing may run on it any more: the role file says
// primary of exactly this group, the instance is stopped, the lease of the group is not this server's and is not
// being adopted, a marker of a hand-over, a fence or a wait is on disk, the plan says that the primary of the group
// is another server, and the catalog term the leader asks under is newer than the one the role file was written
// under. Then the directory becomes a standby in an order that survives a crash at every point: the mark of an
// unfinished copy first, the role file (standby) second, only then the data are touched; the mark names the step, so
// that a rewind that was interrupted is repaired by a base backup and one that finished is not done again.

// ErrOldPrimaryRunning means that the directory of the former primary is running: the leader's demote has to stop it
// first (the fence of last resort of the failover); nothing is rewound under a running instance.
var ErrOldPrimaryRunning = errors.New("standby: the old primary of the group runs")

// ErrNotFormerPrimary means that the directory is a primary that this server cannot prove to be the former primary
// of the group, so it is not touched.
var ErrNotFormerPrimary = fmt.Errorf("%w: the instance is a primary that is not proved to be the former primary of the group", ErrNotProvedStandby)

// OldPrimaryInfo is what the rebuild of an old primary found out and did; the leader keeps it for the report.
type OldPrimaryInfo struct {
	// Method is "pgRewind" or "baseBackup".
	Method string `json:"method"`
	// DivergenceLsn is where the histories parted (pg_rewind --dry-run), OldEndLsn the end of the old primary's WAL
	// (its last checkpoint after crash recovery).
	DivergenceLsn string `json:"divergenceLsn,omitempty"`
	OldEndLsn     string `json:"oldEndLsn,omitempty"`
	// DivergenceBytes is what the old primary wrote after the divergence point (the writes that are discarded);
	// RewindBytes what pg_rewind copied (what it would copy, from the dry run).
	DivergenceBytes *int64 `json:"divergenceBytes,omitempty"`
	RewindBytes     *int64 `json:"rewindBytes,omitempty"`
	Message         string `json:"message,omitempty"`
}

var (
	reDiverged = regexp.MustCompile(`diverged at WAL location ([0-9A-Fa-f]+/[0-9A-Fa-f]+) on timeline (\d+)`)
	reCopy     = regexp.MustCompile(`need to copy (\d+) MB`)
)

// ParseRewind reads what pg_rewind --dry-run --progress printed: the divergence point and the megabytes to copy.
func ParseRewind(out string) (lsn string, mb *int64) {
	if m := reDiverged.FindStringSubmatch(out); m != nil {
		lsn = m[1]
	}
	if m := reCopy.FindStringSubmatch(out); m != nil {
		if n, err := strconv.ParseInt(m[1], 10, 64); err == nil {
			mb = &n
		}
	}
	return lsn, mb
}

// lsnValue turns "X/Y" into the byte position.
func lsnValue(s string) (uint64, bool) {
	hi, lo, ok := strings.Cut(strings.TrimSpace(s), "/")
	if !ok {
		return 0, false
	}
	h, err1 := strconv.ParseUint(hi, 16, 32)
	l, err2 := strconv.ParseUint(lo, 16, 32)
	if err1 != nil || err2 != nil {
		return 0, false
	}
	return h<<32 | l, true
}

// lsnDiff is b - a in bytes, when both parse and b is not before a.
func lsnDiff(a, b string) (int64, bool) {
	x, ok1 := lsnValue(a)
	y, ok2 := lsnValue(b)
	if !ok1 || !ok2 || y < x {
		return 0, false
	}
	return int64(y - x), true
}

// formerPrimaryDir finds the directory that holds the former primary of the group on this server: the one whose role
// file says primary of the group, or standby of the group with the mark of an unfinished copy (an earlier attempt of
// this rebuild). It answers nothing when there are two.
func (a *Agent) formerPrimaryDir(group int32) (int, bool) {
	found := 0
	for _, id := range a.mgr().IDs() {
		r, exists, err := a.mgr().RoleOf(id)
		if err != nil || !exists || r.Group != group {
			continue
		}
		if r.Writable() || (r.IsStandby() && a.mgr().CopyUnfinished(id)) {
			if found != 0 {
				return 0, false
			}
			found = id
		}
	}
	return found, found != 0
}

// dirFor is the directory a rebuild of e works on: the one that holds this standby already, the one the entry names,
// for a diverged rebuild the former primary of the group, else the standby directory of the group.
func (a *Agent) dirFor(e Entry, diverged bool) int {
	for _, id := range a.mgr().IDs() {
		if m, err := a.metaOf(id); err == nil && m.ID == e.ID {
			return id
		}
	}
	if e.InstanceID > 0 {
		return e.InstanceID
	}
	if diverged {
		if id, ok := a.formerPrimaryDir(e.Group); ok {
			return id
		}
	}
	return e.dir()
}

// enterFormer says whether the rebuild of directory id is the one of a former primary, and refuses a primary that is not
// proved to be one. It returns false for a directory that is a standby already (the ordinary rebuild applies) and for
// one that does not exist.
func (a *Agent) enterFormer(ctx context.Context, id int, e Entry, term int64) (bool, error) {
	r, exists, err := a.mgr().RoleOf(id)
	if err != nil {
		return false, fmt.Errorf("%w: %w", ErrNotProvedStandby, err)
	}
	if exists && r.IsStandby() && a.mgr().CopyUnfinished(id) && r.Group == e.Group {
		return true, nil // an earlier attempt of this rebuild: the role file and the mark are the proof
	}
	if !exists || !r.Writable() {
		return false, nil
	}
	if r.Group != e.Group {
		return false, fmt.Errorf("%w: its role file names shard group %d", ErrNotFormerPrimary, r.Group)
	}
	if a.FormerPrimary == nil {
		return false, fmt.Errorf("%w: this server cannot tell whether the lease of the group is held", ErrNotFormerPrimary)
	}
	up, err := a.mgr().Running(ctx, id)
	if err != nil {
		return false, fmt.Errorf("%w: %w", ErrNotFormerPrimary, err)
	}
	if up {
		return false, ErrOldPrimaryRunning
	}
	if e.PrimaryServer == a.Self {
		return false, fmt.Errorf("%w: the plan names this server as the primary of the group", ErrNotFormerPrimary)
	}
	if term <= 0 || term <= r.Term {
		return false, fmt.Errorf("%w: the catalog term %d is not newer than the one of its role file (%d)", ErrNotFormerPrimary, term, r.Term)
	}
	if err := a.FormerPrimary(e.Group); err != nil {
		return false, fmt.Errorf("%w: %w", ErrNotFormerPrimary, err)
	}
	return true, nil
}

// rebuildFormerLocked turns the directory of the former primary of e.Group into the standby e. The caller holds opMu and
// has proved the directory (enterFormer).
func (a *Agent) rebuildFormerLocked(ctx context.Context, id int, e Entry, term int64) (CreateResponse, error) {
	mgr := a.mgr()
	mk, err := mgr.Managed(id)
	if err != nil {
		return CreateResponse{}, err
	}
	slot := SlotName(e.ID)
	conninfo, err := a.replConninfo(e.PrimaryServer, e.PrimaryPort, AppName(e.ID), "")
	if err != nil {
		return CreateResponse{}, fmt.Errorf("standby: building the replication connection: %w", err)
	}
	rewindConninfo, err := a.replConninfo(e.PrimaryServer, e.PrimaryPort, AppName(e.ID), "postgres")
	if err != nil {
		return CreateResponse{}, fmt.Errorf("standby: building the rewind connection: %w", err)
	}
	// 1. The mark first, the role file second: from here on the directory is a standby that is being made, and nothing
	//    but this rebuild may delete or rewrite it. A mark that exists is an earlier attempt: its step stays.
	if !mgr.CopyUnfinished(id) {
		if err := mgr.MarkCopyUnfinished(id); err != nil {
			return CreateResponse{}, fmt.Errorf("standby: marking the directory as an unfinished copy: %w", err)
		}
	}
	role := instance.Role{Group: e.Group, Role: instance.RoleStandby, Origin: instance.OriginCopy, Term: term}
	if cur, exists, rerr := mgr.RoleOf(id); rerr == nil && exists && cur.Term > role.Term {
		role.Term = cur.Term
	}
	if err := mgr.WriteRole(id, role); err != nil {
		return CreateResponse{}, err
	}
	// A standby that is rebuilt because it is ahead of its primary may still be running; a former primary never is.
	if up, rerr := mgr.Running(ctx, id); rerr == nil && up {
		if err := mgr.Stop(ctx, id); err != nil {
			return CreateResponse{}, fmt.Errorf("standby: stopping the instance before it is rewound: %w", err)
		}
	}
	if err := a.slotOnPrimary(ctx, e, slot); err != nil {
		return CreateResponse{}, err
	}
	info := OldPrimaryInfo{}
	method := "pgRewind"
	switch mgr.CopyStep(id) {
	case instance.CopyRewound:
		// The rewind of an earlier attempt finished; only what follows was interrupted.
	case instance.CopyRewinding, instance.CopyBackup:
		method = "baseBackup" // an interrupted pg_rewind leaves the target in an undefined state: only a new copy repairs it
	default:
		// Measure what is about to be discarded, while the directory is still the old primary's.
		info = a.measureDivergence(ctx, id, rewindConninfo)
		_ = mgr.SetCopyStep(id, instance.CopyRewinding)
		lastCheckpoint := ""
		if info.OldEndLsn == "" {
			if cd, cerr := mgr.ControlData(ctx, id); cerr == nil {
				lastCheckpoint = cd["Latest checkpoint location"]
			}
		}
		rerr := error(nil)
		rout := ""
		if a.FailRewind != nil && a.FailRewind() {
			rerr = errors.New("fault injection: pg_rewind is made to fail")
		} else {
			rout, rerr = mgr.Rewind(ctx, id, rewindConninfo)
		}
		if rerr != nil {
			a.Log.Info("standby: pg_rewind of the old primary was not possible, taking a base backup", "group", e.Group, "error", rerr)
			info.Message = "pg_rewind was not possible: " + clipErr(rerr)
			method = "baseBackup"
		} else {
			_ = mgr.SetCopyStep(id, instance.CopyRewound)
			completeFromRewind(&info, rout, lastCheckpoint)
		}
	}
	settings, err := a.standbySettings(id, conninfo, slot)
	if err != nil {
		return CreateResponse{}, err
	}
	if method == "baseBackup" {
		_ = mgr.SetCopyStep(id, instance.CopyBackup)
		if err := mgr.RebaseStandby(ctx, id, instance.StandbySpec{ID: id, Settings: settings, Conninfo: conninfo, Slot: slot}); err != nil {
			return CreateResponse{}, fmt.Errorf("standby: taking a base backup for the old primary of shard group %d: %w", e.Group, err)
		}
	} else if err := mgr.WriteManaged(id, settings); err != nil {
		// pg_rewind copied the managed configuration of the new primary over this one: ours is written again.
		return CreateResponse{}, fmt.Errorf("standby: writing the managed settings of the old primary: %w", err)
	}
	if _, err := a.syncAccess(ctx, id); err != nil {
		return CreateResponse{}, fmt.Errorf("standby: writing the access rules of the old primary: %w", err)
	}
	e.Port, e.InstanceID = mk.Port, id
	if err := a.writeMeta(id, e); err != nil {
		return CreateResponse{}, fmt.Errorf("standby: recording the standby: %w", err)
	}
	// 2. The directory is a standby: the markers that held the old primary back are removed (before it starts, so that
	//    nothing reads them as a hold on a running standby), and the mark of the unfinished copy goes last.
	if a.BecameStandby != nil {
		a.BecameStandby(e.Group, id)
	}
	if err := mgr.ClearCopyUnfinished(id); err != nil {
		return CreateResponse{}, fmt.Errorf("standby: clearing the mark of an unfinished copy: %w", err)
	}
	info.Method = method
	if info.Message != "" {
		info.Message += " "
	}
	info.Message += "The old primary was rebuilt as a standby by " + map[string]string{"pgRewind": "pg_rewind", "baseBackup": "a new base backup"}[method] + "."
	a.audit(ctx, "oldPrimaryRebuilt", strconv.FormatInt(e.ID, 10), audit.OutcomeSuccess, map[string]any{"group": e.Group, "server": a.Self, "method": info.Method})
	res, err := a.startExisting(ctx, id, e)
	if err != nil {
		return res, err
	}
	res.InstanceID, res.OldPrimary = id, &info
	return res, nil
}

// standbySettings are the managed settings of the former primary's own instance, made those of a standby of e's
// primary: no synchronous list (it is the primary's business), hot standby, the connection and the slot.
func (a *Agent) standbySettings(id int, conninfo, slot string) (instance.ManagedSettings, error) {
	s, err := a.mgr().Settings(id)
	if err != nil {
		return instance.ManagedSettings{}, fmt.Errorf("standby: reading the managed settings of the old primary: %w", err)
	}
	s.Extra = maps.Clone(s.Extra)
	if s.Extra == nil {
		s.Extra = map[string]string{}
	}
	delete(s.Extra, syncNamesKey)
	s.Extra["hot_standby"] = "on"
	s.Extra["primary_conninfo"] = conninfo
	s.Extra["primary_slot_name"] = slot
	return s, nil
}

// measureDivergence reads what the old primary holds that the new one does not, before it is discarded: pg_rewind
// --dry-run says where the histories parted and how much would be copied, and the shutdown checkpoint it leaves (crash
// recovery is finished first) is the end of the old primary's WAL. Every part is best effort: a report without a
// number is better than a rebuild that does not happen.
func (a *Agent) measureDivergence(ctx context.Context, id int, rewindConninfo string) OldPrimaryInfo {
	var info OldPrimaryInfo
	out, err := a.mgr().RewindDryRun(ctx, id, rewindConninfo)
	if err != nil && strings.Contains(err.Error(), "shut down cleanly") {
		// A primary that was fenced has to finish its crash recovery first: pg_rewind does it, and even with --dry-run it
		// then stops with this refusal. The recovery is done by now: asked again, the target is clean.
		out, err = a.mgr().RewindDryRun(ctx, id, rewindConninfo)
	}
	if err != nil {
		a.Log.Info("standby: measuring the divergence of the old primary failed", "error", err)
		info.Message = "The divergence could not be measured: " + clipErr(err) + "."
		return info
	}
	lsn, mb := ParseRewind(out)
	info.DivergenceLsn = lsn
	if mb != nil {
		b := *mb << 20
		info.RewindBytes = &b
	}
	if cd, err := a.mgr().ControlData(ctx, id); err == nil {
		info.OldEndLsn = cd["Latest checkpoint location"]
	}
	if d, ok := lsnDiff(info.DivergenceLsn, info.OldEndLsn); ok {
		info.DivergenceBytes = &d
	}
	return info
}

func clipErr(err error) string {
	s := err.Error()
	if len(s) > 300 {
		s = s[:300] + "..."
	}
	return s
}

// replConninfo is the replication connection string to a primary (a test replaces it).
func (a *Agent) replConninfo(host string, port int, app, db string) (string, error) {
	if a.Conninfo != nil {
		return a.Conninfo(host, port, app, db)
	}
	return a.Svc.ReplicationConninfo(host, port, app, db)
}

// slotOnPrimary makes sure the slot of the standby exists on its primary (a test replaces it).
func (a *Agent) slotOnPrimary(ctx context.Context, e Entry, slot string) error {
	if a.EnsureSlot != nil {
		return a.EnsureSlot(ctx, e, slot)
	}
	return a.ensureSlot(ctx, e, slot)
}

// completeFromRewind fills what the dry run could not tell (a primary that was fenced has to finish its crash recovery
// first, and pg_rewind --dry-run then refuses) from what the rewind itself printed: where the histories parted and the
// megabytes it copied. The end of the old WAL is then only the last checkpoint that the directory had before, so the
// bytes written after the divergence are a lower bound, and the message says so.
func completeFromRewind(info *OldPrimaryInfo, out, lastCheckpoint string) {
	if info.DivergenceLsn != "" {
		return
	}
	lsn, mb := ParseRewind(out)
	if lsn == "" {
		return
	}
	info.DivergenceLsn = lsn
	if mb != nil && info.RewindBytes == nil {
		b := *mb << 20
		info.RewindBytes = &b
	}
	info.Message = "The histories parted at " + lsn + "; the dry run could not measure the old WAL (the instance needed crash recovery), so what follows is the divergence the rewind itself reported."
	if lastCheckpoint != "" {
		info.OldEndLsn = lastCheckpoint
		if d, ok := lsnDiff(lsn, lastCheckpoint); ok {
			info.DivergenceBytes = &d
			info.Message += " The bytes written after the divergence are at least the distance to the last checkpoint."
		}
	}
}
