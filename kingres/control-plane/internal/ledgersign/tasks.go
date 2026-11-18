// SPDX-License-Identifier: AGPL-3.0-only

package ledgersign

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/api"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/audit"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/ledgerkeys"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/ledgerlog"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/problem"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/scheduler"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/task"
)

func ledgerlogSign(ctx context.Context, text []byte, name string, signers []ledgerkeys.Signer) ([]byte, error) {
	n, err := ledgerlog.SignNote(ctx, text, name, signers...)
	if err != nil {
		return nil, err
	}
	return n.Marshal()
}

// staleNext is the age beyond which a next key is not taken again: the library refuses to activate a key more than
// MaxActivationLeadDays after its creation, and the activation is recorded some time after the key is adopted.
const staleNext = 300 * 24 * time.Hour

// keyInput is the input of a key task. It holds no secret: a fingerprint, a time, a reason and two numbers.
type keyInput struct {
	OverlapDays      int       `json:"overlapDays,omitempty"`
	BackupMade       bool      `json:"backupMade,omitempty"`
	Fingerprint      string    `json:"fingerprint,omitempty"`
	CompromisedSince time.Time `json:"compromisedSince,omitempty"`
	Reason           string    `json:"reason,omitempty"`
}

func decodeInput(raw json.RawMessage) (keyInput, error) {
	var in keyInput
	if len(raw) == 0 {
		return in, nil
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return in, &coded{code: "bad_input", msg: "the input of the task is not valid"}
	}
	return in, nil
}

// Definitions returns the task definitions of this package.
func (s *Service) Definitions() []task.Definition {
	gen := task.Step{Name: "Generate the key", Run: s.stepGenerate}
	dist := task.Step{Name: "Give the key to the servers that may become the leader", Run: s.stepInstall}
	added := task.Step{Name: "Publish the key-added event", Run: s.stepEventAdded}
	activate := task.Step{Name: "Activate the key", Irreversible: true, Run: s.stepActivate}
	resign := task.Step{Name: "Sign the latest checkpoint of every log again", Run: s.stepResign}
	remove := task.Step{Name: "Delete the key files that no server needs", Run: s.stepRemove}
	return []task.Definition{
		{Kind: KindCreate, Steps: []task.Step{gen, dist, added, activate}},
		{Kind: KindRotate, Steps: []task.Step{gen, dist, added, activate, resign}},
		{Kind: KindRetire, Steps: []task.Step{
			{Name: "Publish the key-retired event", Run: s.stepEventRetired},
			{Name: "Retire the key", Irreversible: true, Run: s.stepRetire},
			resign, remove,
		}},
		{Kind: KindRevoke, Steps: []task.Step{
			{Name: "Publish the key-revoked event", Run: s.stepEventRevoked},
			{Name: "Revoke the key", Irreversible: true, Run: s.stepRevoke},
			resign, remove,
		}},
		{Kind: KindDistribute, Steps: []task.Step{dist, {Name: "Delete the key files that no server needs", Run: s.stepRemoveLenient}}},
	}
}

// ---------------------------------------------------------------------------------------------------------
// The operations that start a task.

func (s *Service) ready() error {
	if _, ok := s.cfg.Term(); !ok {
		return problem.New(problem.ServiceUnavailable, "The manager has just started; try again in a few seconds.").WithRetryAfter(5)
	}
	return nil
}

func (s *Service) start(ctx context.Context, kind, actor string, in keyInput) (task.Task, error) {
	if s.cfg.StartTask == nil {
		return task.Task{}, problem.New(problem.UnsupportedOp, "This server cannot run tasks.")
	}
	t, err := s.cfg.StartTask(ctx, kind, SubjectType, SubjectID, in, actor)
	if errors.Is(err, task.ErrSubjectBusy) {
		return t, problem.New(problem.OperationInProgress, "A task on the signing keys is running; wait for it to end.")
	}
	return t, err
}

// CreateKey is createLedgerSigningKey: the first key (ledger.key.create), or a rotation (ledger.key.rotate).
func (s *Service) CreateKey(ctx context.Context, actor string, req api.CreateLedgerSigningKeyRequest) (task.Task, error) {
	if err := s.ready(); err != nil {
		return task.Task{}, err
	}
	var errs []problem.FieldError
	in := keyInput{OverlapDays: ledgerkeys.DefaultOverlapDays}
	if req.OverlapDays != nil {
		if *req.OverlapDays < ledgerkeys.MinOverlapDays || *req.OverlapDays > ledgerkeys.MaxOverlapDays {
			errs = append(errs, fieldErr("/overlapDays", "must be 1 to 365", "out_of_range"))
		}
		in.OverlapDays = *req.OverlapDays
	}
	if req.BackupMade != nil {
		in.BackupMade = *req.BackupMade
	}
	if len(errs) > 0 {
		return task.Task{}, problem.Validation("The request is not valid.", errs...)
	}
	if req.PublicKey != nil {
		return task.Task{}, problem.New(problem.UnsupportedOp, "The signing mode command (an external signer with its own public key) is not supported by this version; the keys are made by kingresd (mode file).")
	}
	v, err := s.load(ctx)
	if err != nil {
		return task.Task{}, err
	}
	if v.alarm != nil {
		return task.Task{}, problem.New(problem.Conflict, "The stored key set is damaged and no key task can start: "+v.alarm.Error())
	}
	kind := KindCreate
	if k := activeCount(v.set); k > 0 || retiredOrRevoked(v.set) {
		kind = KindRotate
		if k >= ledgerkeys.MaxActive {
			return task.Task{}, problem.New(problem.Conflict, "A rotation is under way: two keys are active. Wait for the overlap to end or retire the older key.")
		}
		if k == 0 {
			return task.Task{}, problem.New(problem.SigningKeyMissing, "No key is active, so there is no key that can authorise a new one.")
		}
	}
	if len(v.set) >= ledgerkeys.MaxKeys {
		return task.Task{}, problem.New(problem.Conflict, "The key set is full (16 keys, the most the trust file allows); a new generation of the trust file under a new origin prefix is needed.")
	}
	return s.start(ctx, kind, actor, in)
}

// RetireKey is retireLedgerSigningKey.
func (s *Service) RetireKey(ctx context.Context, actor, fp string) (task.Task, error) {
	if err := s.ready(); err != nil {
		return task.Task{}, err
	}
	v, k, err := s.findKey(ctx, fp)
	if err != nil {
		return task.Task{}, err
	}
	if k.State != ledgerkeys.StateActive {
		return task.Task{}, problem.New(problem.Conflict, "Only an active key can be retired; this key is "+k.State.String()+".")
	}
	now, err := s.clock(ctx)
	if err != nil {
		return task.Task{}, problem.New(problem.ServiceUnavailable, err.Error()).WithRetryAfter(30)
	}
	if len(activeSigners(v.set, now, fp)) == 0 {
		return task.Task{}, problem.New(problem.LastSigningKey, "No other key is active and in force; create a key first, the cluster must always be able to sign.")
	}
	return s.start(ctx, KindRetire, actor, keyInput{Fingerprint: fp})
}

// RevokeKey is revokeLedgerSigningKey.
func (s *Service) RevokeKey(ctx context.Context, actor, fp string, req api.RevokeLedgerSigningKeyRequest) (task.Task, error) {
	if err := s.ready(); err != nil {
		return task.Task{}, err
	}
	var errs []problem.FieldError
	if req.Confirm != fp {
		errs = append(errs, fieldErr("/confirm", "must be the fingerprint of the key", "mismatch"))
	}
	if err := ValidReason(req.Reason); err != nil {
		errs = append(errs, fieldErr("/reason", err.Error(), "pattern"))
	}
	if req.CompromisedSince.IsZero() {
		errs = append(errs, fieldErr("/compromisedSince", "is required", "required"))
	}
	if len(errs) > 0 {
		return task.Task{}, problem.Validation("The request is not valid.", errs...)
	}
	v, k, err := s.findKey(ctx, fp)
	if err != nil {
		return task.Task{}, err
	}
	if k.State == ledgerkeys.StateRevoked {
		return task.Task{}, problem.New(problem.Conflict, "The key is revoked already.")
	}
	now, err := s.clock(ctx)
	if err != nil {
		return task.Task{}, problem.New(problem.ServiceUnavailable, err.Error()).WithRetryAfter(30)
	}
	if req.CompromisedSince.After(now.Add(s.cfg.MaxClockSkew)) {
		return task.Task{}, problem.Validation("The request is not valid.", fieldErr("/compromisedSince", "must not be later than now", "out_of_range"))
	}
	if len(activeSigners(v.set, now, fp)) == 0 {
		return task.Task{}, problem.New(problem.LastSigningKey, "No other key is active and in force to sign the revocation; create a key first (the daemon does not make one inside a revocation).")
	}
	return s.start(ctx, KindRevoke, actor, keyInput{Fingerprint: fp, CompromisedSince: req.CompromisedSince.UTC().Truncate(time.Second), Reason: req.Reason})
}

func (s *Service) findKey(ctx context.Context, fp string) (*view, ledgerkeys.Key, error) {
	if !fingerprintOK(fp) {
		return nil, ledgerkeys.Key{}, problem.New(problem.NotFound, "No such key.")
	}
	v, err := s.load(ctx)
	if err != nil {
		return nil, ledgerkeys.Key{}, err
	}
	if v.alarm != nil {
		return nil, ledgerkeys.Key{}, problem.New(problem.Conflict, "The stored key set is damaged and no key task can start: "+v.alarm.Error())
	}
	k, ok := v.set.Get(fp)
	if !ok {
		return nil, ledgerkeys.Key{}, problem.New(problem.NotFound, "No such key.")
	}
	return v, k, nil
}

// ---------------------------------------------------------------------------------------------------------
// The steps. Each one reads the state again and does what is not done: a repeated step changes nothing.

// stepGenerate makes the new key: it adopts the key this task made before it was interrupted, or a key that a
// cancelled task left in state next, or an orphan file this task wrote before the row existed, and otherwise
// generates one. A key that would have the key id of another is dropped before anything is written
// (AddChecked), and another is generated.
func (s *Service) stepGenerate(ctx context.Context, env *task.Env) error {
	return s.mutate(ctx, func(v *view, now time.Time) (*Change, error) {
		taskID := env.Task.ID
		for _, k := range v.snap.Keys {
			if k.TaskID == taskID {
				return nil, nil // made by this task already
			}
		}
		cl := v.snap.Cluster
		if cl.ClusterID == nil {
			id, err := newClusterID()
			if err != nil {
				return nil, err
			}
			cl.ClusterID = id
		}
		if cl.OriginPrefix == "" {
			cl.OriginPrefix = v.prefix
		}
		if env.Task.Kind == KindCreate || env.Task.Kind == KindRotate {
			in, err := decodeInput(env.Task.Input)
			if err != nil {
				return nil, err
			}
			if in.BackupMade {
				cl.BackupAdvised = false
			} else {
				cl.BackupAdvised = true
			}
		}
		// A key that a cancelled or failed task left in state next, with its file here and no event. A key that
		// has waited for most of the 366 days after which the library refuses to activate it is left alone (the
		// operator may revoke it once another key is active) and a fresh one is made.
		for _, k := range v.snap.Keys {
			if k.State == ledgerkeys.StateNext && now.Sub(k.CreatedAt) < staleNext && s.cfg.Keyring.Has(k.Fingerprint()) {
				if _, ok := s.eventExists(v, EventKeyAdded, k.Fingerprint()); !ok {
					k.TaskID = taskID
					return &Change{Cluster: &cl, Keys: []KeyRow{k}}, nil
				}
			}
		}
		known := map[string]bool{}
		for _, k := range v.set {
			known[k.Fingerprint()] = true
		}
		t := recordTime(v.set, now)
		origins, err := s.origins(ctx, v.prefix)
		if err != nil {
			return nil, err
		}
		// The file this task wrote just before it was killed.
		if fp, err := s.cfg.Keyring.Orphan(known, env.Task.CreatedAt, s.cfg.MaxClockSkew); err == nil && fp != "" {
			if pub, err := s.cfg.Keyring.PublicOf(fp); err == nil {
				if set, err := v.set.AddChecked(pub, "", t, origins...); err == nil {
					return &Change{Cluster: &cl, Keys: rows(v.snap.Keys, set, taskID, fp)}, nil
				}
			}
		}
		for try := 0; try < 8; try++ {
			pub, write, drop, err := s.cfg.Keyring.Generate()
			if err != nil {
				return nil, err
			}
			set, err := v.set.AddChecked(pub, "", t, origins...)
			if errors.Is(err, ledgerkeys.ErrKeyIDCollision) {
				drop() // nothing was written; make another
				continue
			}
			if err != nil {
				drop()
				return nil, &coded{code: "cannot_add_key", msg: err.Error()}
			}
			// The file first: a row without a file could not be repaired, a file without a row is found
			// again by Orphan.
			if err := write(); err != nil {
				return nil, err
			}
			return &Change{Cluster: &cl, Keys: rows(v.snap.Keys, set, taskID, ledgerkeys.Fingerprint(pub))}, nil
		}
		return nil, &coded{code: "key_id_collision", msg: "eight keys in a row had a key id that another key of the set has; try again"}
	})
}

// newKey is the key a create or rotate task made.
func newKey(v *view, taskID int64) (ledgerkeys.Key, bool) {
	for _, k := range v.snap.Keys {
		if k.TaskID == taskID {
			return k.Key, true
		}
	}
	return ledgerkeys.Key{}, false
}

func (s *Service) stepInstall(ctx context.Context, env *task.Env) error {
	return s.install(ctx, env)
}

// stepRemove is the step of a retirement or a revocation: a key file of a key that is over, left on a server, is a
// secret that should not exist, so a server that cannot be asked or cleaned fails the task (retryable).
func (s *Service) stepRemove(ctx context.Context, env *task.Env) error {
	return s.removeStale(ctx, env, true)
}

// stepRemoveLenient is the step of the distribution: a server that cannot be reached is named in the detail and
// tried again by the next distribution; it does not fail the task.
func (s *Service) stepRemoveLenient(ctx context.Context, env *task.Env) error {
	return s.removeStale(ctx, env, false)
}

// stepEventAdded publishes the key-added event of the new key. For a rotation it is signed by the active key
// (it authorises the new one) and by the new key (it proves possession); for the first key by that key alone.
func (s *Service) stepEventAdded(ctx context.Context, env *task.Env) error {
	return s.mutate(ctx, func(v *view, now time.Time) (*Change, error) {
		k, ok := newKey(v, env.Task.ID)
		if !ok {
			return nil, &coded{code: "no_key", msg: "the task has made no key"}
		}
		fp := k.Fingerprint()
		if _, done := s.eventExists(v, EventKeyAdded, fp); done {
			return nil, nil
		}
		if k.State != ledgerkeys.StateNext && k.State != ledgerkeys.StateActive {
			return nil, &coded{code: "bad_state", msg: "the key is " + k.State.String()}
		}
		t := recordTime(v.set, now)
		authorisers := activeSigners(v.set, now, fp)
		signers := append(authorisers, k)
		ev, err := appendEvent(v, EventKeyAdded, fp, t, []field{{"key", b64(k.Public)}, {"fingerprint", fp}, {"not-before", t.UTC().Format(time.RFC3339)}},
			func(text []byte) ([]byte, error) {
				return s.signEvent(ctx, v, text, ledgerkeys.PurposeKeyAdded, fp, signers, now)
			})
		if err != nil {
			return nil, err
		}
		return &Change{Events: []EventRow{ev}}, nil
	})
}

// activationTime is the time the key-added event gives as not-before: the key is activated at that time, so
// the trust file says what the event says.
func (s *Service) activationTime(v *view, fp string) (time.Time, error) {
	e, ok := s.eventExists(v, EventKeyAdded, fp)
	if !ok {
		return time.Time{}, &coded{code: "no_event", msg: "the key-added event of the key is missing"}
	}
	f, err := eventFields(noteText(e.Note))
	if err != nil {
		return time.Time{}, &coded{code: "bad_event", msg: err.Error()}
	}
	t, err := time.Parse(time.RFC3339, f["not-before"])
	if err != nil {
		return time.Time{}, &coded{code: "bad_event", msg: "the not-before of the event is not a time"}
	}
	return t.UTC(), nil
}

// stepActivate makes the key active, at the time of its event, and publishes the trust file.
func (s *Service) stepActivate(ctx context.Context, env *task.Env) error {
	in, err := decodeInput(env.Task.Input)
	if err != nil {
		return err
	}
	return s.mutate(ctx, func(v *view, now time.Time) (*Change, error) {
		k, ok := newKey(v, env.Task.ID)
		if !ok {
			return nil, &coded{code: "no_key", msg: "the task has made no key"}
		}
		fp := k.Fingerprint()
		if k.State != ledgerkeys.StateNext {
			return nil, nil // active already (or ended)
		}
		at, err := s.activationTime(v, fp)
		if err != nil {
			return nil, err
		}
		days := in.OverlapDays
		if days == 0 {
			days = ledgerkeys.DefaultOverlapDays
		}
		set, err := v.set.Activate(fp, at, days)
		if err != nil {
			return nil, &coded{code: "cannot_activate", msg: err.Error()}
		}
		trust, err := s.trustFor(ctx, v.prefix, set)
		if err != nil {
			return nil, &coded{code: "trust_file", msg: err.Error()}
		}
		cl := v.snap.Cluster
		cl.TrustFile = trust
		return &Change{Cluster: &cl, Keys: rows(v.snap.Keys, set, 0, "")}, nil
	})
}

// stepEventRetired publishes the key-retired event, signed by another active key. The bound of the retired
// key (the largest tree it may still sign) is decided here and written in the event: at least the largest size
// of any stored checkpoint, and at least 1 (a key that signed nothing is bound to the smallest tree the
// verifier accepts, so a key that leaks later signs nothing the verifier takes).
func (s *Service) stepEventRetired(ctx context.Context, env *task.Env) error {
	in, err := decodeInput(env.Task.Input)
	if err != nil {
		return err
	}
	max, err := s.cfg.Store.MaxCheckpointSize(ctx)
	if err != nil {
		return err
	}
	if max < 1 {
		max = 1
	}
	return s.mutate(ctx, func(v *view, now time.Time) (*Change, error) {
		k, ok := v.set.Get(in.Fingerprint)
		if !ok {
			return nil, &coded{code: "no_key", msg: "no such key"}
		}
		if _, done := s.eventExists(v, EventKeyRetired, in.Fingerprint); done {
			return nil, nil
		}
		if k.State != ledgerkeys.StateActive {
			return nil, &coded{code: "bad_state", msg: "the key is " + k.State.String()}
		}
		others := activeSigners(v.set, now, in.Fingerprint)
		if len(others) == 0 {
			return nil, &coded{code: "last_signing_key", msg: "no other key is active and in force"}
		}
		t := recordTime(v.set, now)
		ev, err := appendEvent(v, EventKeyRetired, in.Fingerprint, t, []field{{"key", b64(k.Public)}, {"max-tree-size", strconv.FormatUint(max, 10)}},
			func(text []byte) ([]byte, error) {
				return s.signEvent(ctx, v, text, ledgerkeys.PurposeKeyRetired, in.Fingerprint, others[:1], now)
			})
		if err != nil {
			return nil, err
		}
		return &Change{Events: []EventRow{ev}}, nil
	})
}

// stepRetire retires the key with the bound its event states.
func (s *Service) stepRetire(ctx context.Context, env *task.Env) error {
	in, err := decodeInput(env.Task.Input)
	if err != nil {
		return err
	}
	return s.mutate(ctx, func(v *view, now time.Time) (*Change, error) {
		k, ok := v.set.Get(in.Fingerprint)
		if !ok {
			return nil, &coded{code: "no_key", msg: "no such key"}
		}
		if k.State == ledgerkeys.StateRetired {
			return nil, nil
		}
		e, ok := s.eventExists(v, EventKeyRetired, in.Fingerprint)
		if !ok {
			return nil, &coded{code: "no_event", msg: "the key-retired event is missing"}
		}
		f, err := eventFields(noteText(e.Note))
		if err != nil {
			return nil, &coded{code: "bad_event", msg: err.Error()}
		}
		size, err := strconv.ParseUint(f["max-tree-size"], 10, 64)
		if err != nil || size < 1 {
			return nil, &coded{code: "bad_event", msg: "the max-tree-size of the event is not a size"}
		}
		set, err := v.set.Retire(in.Fingerprint, recordTime(v.set, now), ledgerkeys.AtMost(size))
		if err != nil {
			return nil, &coded{code: "cannot_retire", msg: err.Error()}
		}
		trust, err := s.trustFor(ctx, v.prefix, set)
		if err != nil {
			return nil, &coded{code: "trust_file", msg: err.Error()}
		}
		cl := v.snap.Cluster
		cl.TrustFile = trust
		return &Change{Cluster: &cl, Keys: rows(v.snap.Keys, set, 0, "")}, nil
	})
}

func (s *Service) stepEventRevoked(ctx context.Context, env *task.Env) error {
	in, err := decodeInput(env.Task.Input)
	if err != nil {
		return err
	}
	return s.mutate(ctx, func(v *view, now time.Time) (*Change, error) {
		k, ok := v.set.Get(in.Fingerprint)
		if !ok {
			return nil, &coded{code: "no_key", msg: "no such key"}
		}
		if _, done := s.eventExists(v, EventKeyRevoked, in.Fingerprint); done {
			return nil, nil
		}
		if k.State == ledgerkeys.StateRevoked {
			return nil, nil
		}
		others := activeSigners(v.set, now, in.Fingerprint)
		if len(others) == 0 {
			return nil, &coded{code: "last_signing_key", msg: "no other key is active and in force"}
		}
		t := recordTime(v.set, now)
		since := clampSince(in.CompromisedSince, t, k)
		ev, err := appendEvent(v, EventKeyRevoked, in.Fingerprint, t,
			[]field{{"key", b64(k.Public)}, {"compromised-since", since.UTC().Format(time.RFC3339)}, {"reason", in.Reason}},
			func(text []byte) ([]byte, error) {
				return s.signEvent(ctx, v, text, ledgerkeys.PurposeKeyRevoked, in.Fingerprint, others[:1], now)
			})
		if err != nil {
			return nil, err
		}
		return &Change{Events: []EventRow{ev}}, nil
	})
}

// clampSince is the compromised-since of a revocation as the event and the key record it: never later than the time
// of the revocation (an earlier time is the safe direction, and the library refuses a later one) and never
// before the creation of the key (a key cannot be compromised before it exists; the library refuses it).
func clampSince(since, t time.Time, k ledgerkeys.Key) time.Time {
	since = since.UTC().Truncate(time.Second)
	if since.After(t) {
		since = t
	}
	if since.Before(k.CreatedAt) {
		since = k.CreatedAt
	}
	return since
}

func (s *Service) stepRevoke(ctx context.Context, env *task.Env) error {
	in, err := decodeInput(env.Task.Input)
	if err != nil {
		return err
	}
	return s.mutate(ctx, func(v *view, now time.Time) (*Change, error) {
		k, ok := v.set.Get(in.Fingerprint)
		if !ok {
			return nil, &coded{code: "no_key", msg: "no such key"}
		}
		if k.State == ledgerkeys.StateRevoked {
			return nil, nil
		}
		t := recordTime(v.set, now)
		since := clampSince(in.CompromisedSince, t, k)
		set, err := v.set.Revoke(in.Fingerprint, t, since, in.Reason)
		if err != nil {
			return nil, &coded{code: "cannot_revoke", msg: err.Error()}
		}
		trust, err := s.trustFor(ctx, v.prefix, set)
		if err != nil {
			return nil, &coded{code: "trust_file", msg: err.Error()}
		}
		cl := v.snap.Cluster
		cl.TrustFile = trust
		cl.BackupAdvised = true
		return &Change{Cluster: &cl, Keys: rows(v.snap.Keys, set, 0, "")}, nil
	})
}

// stepResign hands the new set to the collector, which signs the latest checkpoint of every log again with
// the keys that sign now (correction 47: a log that stops growing would otherwise keep only the signature of
// the old key). Until the collector exists (the next step of the plan) no log is published and the step
// is recorded as skipped.
func (s *Service) stepResign(ctx context.Context, _ *task.Env) error {
	if s.cfg.Resign == nil {
		return task.Skip("no log is published yet, there is no checkpoint to sign again")
	}
	v, err := s.load(ctx)
	if err != nil {
		return err
	}
	if v.alarm != nil {
		return &coded{code: "key_set_damaged", msg: v.alarm.Error()}
	}
	return s.cfg.Resign(ctx, v.set)
}

// ---------------------------------------------------------------------------------------------------------
// Distribution (3.2.2): the key goes to the servers that run an instance of shard group 0, and leaves a server
// that does not run one.

// callServer runs an internal call on a server; a call to this server stays in the process.
func (s *Service) callServer(ctx context.Context, server, key string, in any, out any) error {
	if server == s.cfg.Self {
		body, err := json.Marshal(in)
		if err != nil {
			return err
		}
		res, err := s.agent.Handle(ctx, key, body)
		if err != nil {
			return err
		}
		if out != nil {
			b, err := json.Marshal(res)
			if err != nil {
				return err
			}
			return json.Unmarshal(b, out)
		}
		return nil
	}
	if s.cfg.Call == nil {
		return errors.New("ledgersign: this server cannot call other servers")
	}
	method, path, _ := strings.Cut(key, " ")
	return s.cfg.Call(ctx, server, method, path, in, out)
}

// wanted are the keys whose file belongs on a holder: next and active keys of the mode file.
func wanted(v *view) map[string]bool {
	w := map[string]bool{}
	for _, k := range v.snap.Keys {
		if k.Mode == "file" && (k.State == ledgerkeys.StateNext || k.State == ledgerkeys.StateActive) {
			w[k.Fingerprint()] = true
		}
	}
	return w
}

// install gives every wanted key to every holder that lacks it. A holder that cannot be reached does not
// fail the step: the leader holds the key, and the job ledger.keys.sync starts the distribution again; the
// servers still missing are named in the detail. This server must hold every wanted key.
func (s *Service) install(ctx context.Context, env *task.Env) error {
	v, err := s.load(ctx)
	if err != nil {
		return err
	}
	if v.alarm != nil {
		return &coded{code: "key_set_damaged", msg: v.alarm.Error()}
	}
	holders, err := s.holderSet(ctx)
	if err != nil {
		return fmt.Errorf("ledgersign: finding the servers of shard group 0: %w", err)
	}
	want := wanted(v)
	var pending []string
	var done []string
	for _, h := range holders {
		if h == s.cfg.Self {
			for fp := range want {
				if !s.cfg.Keyring.Has(fp) {
					return &coded{code: "key_file_missing", msg: "this server does not have the key file of the key " + fp[:16]}
				}
			}
			done = append(done, h)
			continue
		}
		if err := s.installOn(ctx, h, want); err != nil {
			s.log.Warn("a server could not be given the signing keys", "server", h, "error", err)
			pending = append(pending, h)
			continue
		}
		done = append(done, h)
	}
	if env != nil && len(pending) > 0 {
		_ = env.SetDetail(ctx, "the signing keys are not on: "+strings.Join(pending, ", ")+" (they are tried again)")
	}
	// A server that was a holder stays recorded until its files were removed (removeStale), so that the
	// removal still knows where to look.
	return s.recordHolders(ctx, union(done, v.snap.Cluster.Holders))
}

func (s *Service) installOn(ctx context.Context, server string, want map[string]bool) error {
	var have ListResponse
	if err := s.callServer(ctx, server, CallList, struct{}{}, &have); err != nil {
		return err
	}
	present := map[string]bool{}
	for _, fp := range have.Fingerprints {
		present[fp] = true
	}
	for fp := range want {
		if present[fp] {
			continue
		}
		data, err := s.cfg.Keyring.File(fp)
		if err != nil {
			return err
		}
		req := InstallRequest{Fingerprint: fp, File: b64(data)}
		clear(data)
		err = s.callServer(ctx, server, CallInstall, req, nil)
		req.File = ""
		if err != nil {
			return err
		}
	}
	return nil
}

// removeStale deletes the files of keys that are not wanted on a server: every file of a retired or revoked key
// on any server that was or is a holder, and every file on a server that stopped being one.
func (s *Service) removeStale(ctx context.Context, env *task.Env, strict bool) error {
	v, err := s.load(ctx)
	if err != nil {
		return err
	}
	holders, err := s.holderSet(ctx)
	if err != nil {
		return fmt.Errorf("ledgersign: finding the servers of shard group 0: %w", err)
	}
	isHolder := map[string]bool{}
	servers := map[string]bool{}
	for _, h := range holders {
		isHolder[h] = true
		servers[h] = true
	}
	for _, h := range v.snap.Cluster.Holders {
		servers[h] = true
	}
	want := wanted(v)
	var pending []string
	var good []string // the holders that have every wanted key
	for h := range servers {
		var have ListResponse
		if err := s.callServer(ctx, h, CallList, struct{}{}, &have); err != nil {
			s.log.Warn("the key files of a server could not be listed", "server", h, "error", err)
			pending = append(pending, h)
			continue
		}
		failed := false
		present := map[string]bool{}
		for _, fp := range have.Fingerprints {
			present[fp] = true
			if isHolder[h] && want[fp] {
				continue
			}
			if err := s.callServer(ctx, h, CallRemove, RemoveRequest{Fingerprint: fp}, nil); err != nil {
				s.log.Warn("a key file could not be removed", "server", h, "error", err)
				failed = true
			}
		}
		if failed {
			pending = append(pending, h)
			continue
		}
		if isHolder[h] {
			all := true
			for fp := range want {
				all = all && present[fp]
			}
			if all {
				good = append(good, h)
			}
		}
	}
	if len(pending) > 0 {
		sortStrings(pending)
		if env != nil {
			_ = env.SetDetail(ctx, "key files may remain on: "+strings.Join(pending, ", ")+" (they are removed when the server answers)")
		}
		if !strict {
			return s.recordHolders(ctx, union(good, pending))
		}
		// A retired or revoked key's file left on a server is a secret that should not exist: the task
		// does not report success while one is known to remain.
		return &coded{code: "key_files_remain", msg: "a key file that no server needs could not be removed from: " + strings.Join(pending, ", "), retryable: true}
	}
	// Every file is where it belongs: the holders on record are the holders that have the keys.
	return s.recordHolders(ctx, good)
}

func union(a, b []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, x := range append(append([]string(nil), a...), b...) {
		if !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}
	return out
}

func sortStrings(a []string) {
	for i := 1; i < len(a); i++ {
		for j := i; j > 0 && a[j] < a[j-1]; j-- {
			a[j], a[j-1] = a[j-1], a[j]
		}
	}
}

// recordHolders stores the servers that hold the keys.
func (s *Service) recordHolders(ctx context.Context, holders []string) error {
	sortStrings(holders)
	return s.mutate(ctx, func(v *view, _ time.Time) (*Change, error) {
		if strings.Join(v.snap.Cluster.Holders, ",") == strings.Join(holders, ",") {
			return nil, nil
		}
		cl := v.snap.Cluster
		cl.Holders = holders
		return &Change{Cluster: &cl}, nil
	})
}

// ---------------------------------------------------------------------------------------------------------
// The jobs of the scheduler.

// Jobs returns the jobs of this package for the scheduler: the retirement of a key whose overlap ended, the
// check of the key set that raises the alert ledgerSignerUnavailable, and the distribution of the keys to the
// servers that became holders.
func (s *Service) Jobs() []scheduler.Job {
	return []scheduler.Job{
		{Name: "ledger.key.due", Every: time.Minute, Jitter: 0.1, Run: s.jobDue},
		{Name: "ledger.signer.check", Every: 10 * time.Second, Jitter: 0.1, Run: s.jobCheck},
		{Name: "ledger.key.sync", Every: time.Minute, Jitter: 0.1, Run: s.jobSync},
	}
}

// jobDue retires an active key whose overlap has ended (DueForRetirement), by the task ledger.key.retire.
// Nothing is retired while the set is damaged or a key task is running.
func (s *Service) jobDue(ctx context.Context, term scheduler.Term) error {
	v, err := s.load(ctx)
	if err != nil || v.alarm != nil {
		return err
	}
	now, err := s.clock(ctx)
	if err != nil {
		return nil // no time is taken from a clock that cannot be compared
	}
	for _, fp := range v.set.DueForRetirement(now) {
		if s.cfg.StartTask == nil {
			return nil
		}
		t, err := s.cfg.StartTask(ctx, KindRetire, SubjectType, SubjectID, keyInput{Fingerprint: fp}, "system")
		if errors.Is(err, task.ErrSubjectBusy) {
			return nil
		}
		if err != nil {
			return err
		}
		if s.cfg.Audit != nil {
			s.cfg.Audit(context.WithoutCancel(ctx), audit.Event{Actor: "system", Source: "system", Action: "retireLedgerSigningKey", ResourceType: SubjectType,
				ResourceID: fp, Outcome: audit.OutcomeSuccess, Details: map[string]any{"taskId": t.ID, "reason": "the overlap of the rotation ended"}})
		}
		return nil // one key at a time
	}
	return nil
}

// jobCheck reconciles the alert ledgerSignerUnavailable with the state of the key set: raised when the set is
// damaged, differs from the trust file published, or an active key has no file here; cleared otherwise.
func (s *Service) jobCheck(ctx context.Context, _ scheduler.Term) error {
	if s.cfg.Alert == nil {
		return nil
	}
	v, err := s.load(ctx)
	if err != nil {
		return err
	}
	if a := s.alarmText(v); a != "" {
		s.cfg.Alert(ctx, "ledgerSignerUnavailable", "critical", "cluster", "The ledger cannot be signed: "+a, true)
		return nil
	}
	s.cfg.Alert(ctx, "ledgerSignerUnavailable", "critical", "cluster", "", false)
	return nil
}

// jobSync starts ledger.key.distribute when a holder lacks a wanted key or a server has a file it should not.
func (s *Service) jobSync(ctx context.Context, _ scheduler.Term) error {
	v, err := s.load(ctx)
	if err != nil || v.alarm != nil || len(v.set) == 0 {
		return err
	}
	holders, err := s.holderSet(ctx)
	if err != nil {
		return err
	}
	if len(holders) == 1 && len(v.snap.Cluster.Holders) <= 1 {
		// One server: it is the only holder, and its own file is checked by jobCheck.
		if strings.Join(holders, ",") == strings.Join(v.snap.Cluster.Holders, ",") {
			return nil
		}
	}
	want := wanted(v)
	servers := map[string]bool{}
	isHolder := map[string]bool{}
	for _, h := range holders {
		servers[h], isHolder[h] = true, true
	}
	for _, h := range v.snap.Cluster.Holders {
		servers[h] = true
	}
	dirty := strings.Join(holders, ",") != strings.Join(v.snap.Cluster.Holders, ",")
	for h := range servers {
		var have ListResponse
		if err := s.callServer(ctx, h, CallList, struct{}{}, &have); err != nil {
			continue // an unreachable server is tried again at the next pass
		}
		present := map[string]bool{}
		for _, fp := range have.Fingerprints {
			present[fp] = true
			if !isHolder[h] || !want[fp] {
				dirty = true
			}
		}
		if isHolder[h] {
			for fp := range want {
				if !present[fp] {
					dirty = true
				}
			}
		}
	}
	if !dirty || s.cfg.StartTask == nil {
		return nil
	}
	_, err = s.cfg.StartTask(ctx, KindDistribute, SubjectType, SubjectID, keyInput{}, "system")
	if errors.Is(err, task.ErrSubjectBusy) {
		return nil
	}
	return err
}

// ---------------------------------------------------------------------------------------------------------
// What the collector of the next step uses.

// Signers returns the keys that sign a checkpoint now, opened, for the collector. A follower never signs
// (ErrNotLeader); a damaged set or a missing file is an alarm (ErrAlarm); a key that is active but not in force
// yet means do not sign (ErrDoNotSign); a clock that cannot be trusted is ErrClockSkew. The signers are for
// one pass: every signature of a checkpoint during an overlap must be made with all of them.
func (s *Service) Signers(ctx context.Context) ([]ledgerkeys.Signer, ledgerkeys.Set, error) {
	if _, ok := s.cfg.Term(); !ok {
		return nil, nil, ErrNotLeader
	}
	v, err := s.load(ctx)
	if err != nil {
		return nil, nil, err
	}
	if len(v.set) == 0 {
		return nil, nil, ErrNoKeys
	}
	if v.alarm != nil {
		return nil, nil, v.alarm
	}
	now, err := s.clock(ctx)
	if err != nil {
		return nil, nil, err
	}
	keys, err := v.set.Signers(now)
	switch {
	case errors.Is(err, ledgerkeys.ErrNotInForce):
		return nil, nil, fmt.Errorf("%w: %v", ErrDoNotSign, err)
	case errors.Is(err, ledgerkeys.ErrNoActiveKey):
		return nil, nil, fmt.Errorf("%w: %v", ErrAlarm, err)
	case err != nil:
		return nil, nil, err
	}
	out := make([]ledgerkeys.Signer, 0, len(keys))
	for _, k := range keys {
		sg, err := s.cfg.Keyring.Signer(k.Fingerprint())
		if err != nil {
			return nil, nil, fmt.Errorf("%w: the key %s cannot be opened: %v", ErrAlarm, k.Fingerprint()[:16], err)
		}
		if !equalKey(sg.Public(), k.Public) {
			return nil, nil, fmt.Errorf("%w: the key file of %s holds another key", ErrAlarm, k.Fingerprint()[:16])
		}
		out = append(out, sg)
	}
	return out, v.set, nil
}

func equalKey(a, b []byte) bool { return string(a) == string(b) }

// Set returns the key set as stored (for the collector's trust entries), after the checks of a load.
func (s *Service) Set(ctx context.Context) (ledgerkeys.Set, error) {
	v, err := s.load(ctx)
	if err != nil {
		return nil, err
	}
	if v.alarm != nil {
		return nil, v.alarm
	}
	return v.set, nil
}

// activeCount and retiredOrRevoked are small helpers of CreateKey.
func activeCount(set ledgerkeys.Set) int {
	n := 0
	for _, k := range set {
		if k.State == ledgerkeys.StateActive {
			n++
		}
	}
	return n
}

func retiredOrRevoked(set ledgerkeys.Set) bool {
	for _, k := range set {
		if k.State == ledgerkeys.StateRetired || k.State == ledgerkeys.StateRevoked {
			return true
		}
	}
	return false
}
