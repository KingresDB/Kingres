// SPDX-License-Identifier: AGPL-3.0-only

package ledgersign

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/api"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/audit"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/ledgerkeys"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/problem"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/scheduler"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/task"
)

// Task kinds and their subject (contract 0.11.0). One key task at a time, cluster-wide.
const (
	KindCreate     = "ledger.key.create"
	KindRotate     = "ledger.key.rotate"
	KindRetire     = "ledger.key.retire"
	KindRevoke     = "ledger.key.revoke"
	KindDistribute = "ledger.key.distribute"
	SubjectType    = "ledgerKey"
	SubjectID      = "signing"

	// DefaultPrefixName is the last part of the default origin prefix, after the server address.
	DefaultPrefixName = "kingres-ledger"
	// DefaultMaxClockSkew is how far the clock of this server may be from the clock of the database
	// before the daemon refuses to record a time (the second source of the duty of ledgerkeys).
	DefaultMaxClockSkew = 2 * time.Minute
)

// Errors a caller of Signers tests with errors.Is.
var (
	// ErrNotLeader: a follower never signs, and the leader signs only once the scheduler holds a term.
	ErrNotLeader = errors.New("ledgersign: this server does not hold the leader's term, it does not sign")
	// ErrDoNotSign: the key set is fine but no key is in force at this time (a clock behind an activation,
	// an activation still to come). Wait and check the clock; it is not an alarm of a damaged set.
	ErrDoNotSign = errors.New("ledgersign: no key is in force yet, do not sign")
	// ErrAlarm: the stored key set is damaged, or differs from the trust file published, or the key file of
	// an active key is missing. Nothing is signed and the alert ledgerSignerUnavailable is raised.
	ErrAlarm = errors.New("ledgersign: the signing keys are not in a state that can sign")
	// ErrNoKeys: no key has been created yet.
	ErrNoKeys = errors.New("ledgersign: no signing key has been created")
	// ErrClockSkew: the clock of this server and the clock of the database differ by more than the limit.
	ErrClockSkew = errors.New("ledgersign: the clock of this server differs from the clock of the database")
)

// Config configures a Service.
type Config struct {
	// Self is the server id of this server.
	Self string
	// Store keeps the key set; Keyring holds this server's key files.
	Store   Store
	Keyring *Keyring
	// Term returns the term of the scheduler on this server, false when it holds none.
	Term func() (scheduler.Term, bool)
	// Clock is the clock of this server (default time.Now); MaxClockSkew bounds its difference from the
	// database's (default DefaultMaxClockSkew).
	Clock        func() time.Time
	MaxClockSkew time.Duration
	// DefaultPrefix is the origin prefix used until the operator sets one (the server address and
	// DefaultPrefixName, made a valid name).
	DefaultPrefix string
	// Holders returns the servers that run an instance of shard group 0, the candidates of the manager
	// leader (3.2.2). This server is always a holder (it is the leader when it distributes).
	Holders func(ctx context.Context) ([]string, error)
	// Call is the internal call to another server (method, path without /internal/v1, body, answer). A call
	// made inside a task step carries the fence of the run in ctx.
	Call func(ctx context.Context, server, method, path string, in, out any) error
	// StartTask submits a task and runs it in the background.
	StartTask func(ctx context.Context, kind, subjType, subjID string, input any, actor string) (task.Task, error)
	// Resign re-signs the latest checkpoint of every log with the keys that sign after a rotation (design
	// 14, correction 47). The collector of the next step provides it; nil means no log is published yet.
	Resign func(ctx context.Context, set ledgerkeys.Set) error
	// IsMember, when set, tells whether a server is still a member of the cluster. A server that was removed from the cluster and cannot be reached is
	// forgotten as a holder (its key files may remain on that host: the operator wipes it, the log says so), instead of being tried for ever (P7-03).
	IsMember func(ctx context.Context, server string) (bool, error)
	// PublicationFailure, when set, returns why the collector could not sign for two rounds, "" when it can (correction 134): the check job
	// raises ledgerSignerUnavailable for it too, and clears the alert only when neither the key set nor the collector complains.
	PublicationFailure func() string
	// Alert raises or clears an alert of the cluster; Audit appends an audit event (system actions).
	Alert func(ctx context.Context, kind, severity, subject, message string, raise bool)
	// Published is the record of what was published, outside the system database (internal/ledgerstate, P8-01): an event is appended only
	// while the database holds the events that were published, and every stored event is noted. Nil in tests that do not need it.
	Published Published
	// PublishedCheck, when set, is called before an event is appended: the full check of the collector (it pulls the record from the other
	// holders and starts one from the database when there is none).
	PublishedCheck func(ctx context.Context) error
	Audit          func(ctx context.Context, ev audit.Event)
	Log            *slog.Logger
	// ConflictTries is how often a change is read and decided again after a conflict (default 5).
	ConflictTries int
	// TermWait is how long a step waits for the term (default 30 s).
	TermWait time.Duration
}

// Service is the signing keys of the cluster as the daemon drives them.
type Service struct {
	syncMu  sync.Mutex // the state a distribution was last started for (jobSync)
	syncSig string
	syncAt  time.Time
	dueMu   sync.Mutex // when the due retirement was last started (jobDue)
	dueAt   time.Time
	cfg     Config
	log     *slog.Logger
	agent   *Agent
}

// New builds the service.
func New(cfg Config) (*Service, error) {
	if cfg.Self == "" || cfg.Store == nil || cfg.Keyring == nil || cfg.Term == nil || cfg.DefaultPrefix == "" {
		return nil, errors.New("ledgersign: Self, Store, Keyring, Term and DefaultPrefix are required")
	}
	if err := ledgerkeys.ValidPrefix(cfg.DefaultPrefix); err != nil {
		return nil, fmt.Errorf("ledgersign: the default origin prefix: %w", err)
	}
	if cfg.Clock == nil {
		cfg.Clock = time.Now
	}
	if cfg.MaxClockSkew <= 0 {
		cfg.MaxClockSkew = DefaultMaxClockSkew
	}
	if cfg.ConflictTries <= 0 {
		cfg.ConflictTries = 5
	}
	if cfg.TermWait <= 0 {
		cfg.TermWait = 30 * time.Second
	}
	log := cfg.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Service{cfg: cfg, log: log, agent: &Agent{Keyring: cfg.Keyring}}, nil
}

// Close zeroes the keys this server has opened.
func (s *Service) Close() { s.cfg.Keyring.Close() }

// Agent returns the callee side of the internal calls.
func (s *Service) Agent() *Agent { return s.agent }

// ---------------------------------------------------------------------------------------------------------
// The state as loaded, with the checks that follow every load.

type view struct {
	snap  Snapshot
	set   ledgerkeys.Set
	alarm error
	// prefix is the origin prefix in force: the stored one, or the default.
	prefix string
}

func (s *Service) prefixOf(c ClusterRow) string {
	if c.OriginPrefix != "" {
		return c.OriginPrefix
	}
	return s.cfg.DefaultPrefix
}

func (s *Service) trustOptions() ledgerkeys.TrustOptions { return ledgerkeys.TrustOptions{} }

// hasTrustedKey is true when the set has a key the trust file lists (a key that is not next).
func hasTrustedKey(set ledgerkeys.Set) bool {
	for _, k := range set {
		if k.State != ledgerkeys.StateNext {
			return true
		}
	}
	return false
}

// load reads the state and applies the duties of the caller of ledgerkeys that follow a load: Validate (an
// error is an alarm and no signing; ErrNoActiveKey is the alarm of a damaged set) and the comparison of the
// trust file with the one published at the last change (K3-10).
func (s *Service) load(ctx context.Context) (*view, error) {
	snap, err := s.cfg.Store.Load(ctx)
	if err != nil {
		return nil, err
	}
	v := &view{snap: snap, set: snap.Set(), prefix: s.prefixOf(snap.Cluster)}
	if err := v.set.Validate(); err != nil {
		v.alarm = fmt.Errorf("%w: the stored key set is not valid: %w", ErrAlarm, err)
		return v, nil
	}
	if hasTrustedKey(v.set) {
		want, err := ledgerkeys.TrustFile(v.prefix, v.set, s.trustOptions())
		switch {
		case err != nil:
			v.alarm = fmt.Errorf("%w: the trust file cannot be built from the stored key set: %w", ErrAlarm, err)
		case snap.Cluster.TrustFile == nil:
			v.alarm = fmt.Errorf("%w: the stored key set has no published trust file", ErrAlarm)
		case !bytes.Equal(want, snap.Cluster.TrustFile):
			v.alarm = fmt.Errorf("%w: the trust file built from the stored key set differs from the one published at the last change; "+
				"a key time, bound or label was changed outside the daemon", ErrAlarm)
		}
	} else if snap.Cluster.TrustFile != nil {
		v.alarm = fmt.Errorf("%w: a trust file was published and the stored key set has no key it lists", ErrAlarm)
	}
	if v.alarm == nil {
		if err := verifyEvents(v); err != nil {
			v.alarm = fmt.Errorf("%w: %w", ErrAlarm, err)
		}
	}
	return v, nil
}

// clock reads the clock of this server and compares it with the database's. It returns the time in whole
// seconds, or ErrClockSkew. The daemon passes the key library no time it did not read here.
func (s *Service) clock(ctx context.Context) (time.Time, error) {
	now := s.cfg.Clock()
	db, err := s.cfg.Store.DBTime(ctx)
	if err != nil {
		return time.Time{}, fmt.Errorf("ledgersign: the clock cannot be compared with the database's: %w", err)
	}
	d := now.Sub(db)
	if d < 0 {
		d = -d
	}
	if d > s.cfg.MaxClockSkew {
		return time.Time{}, fmt.Errorf("%w by %s (the limit is %s)", ErrClockSkew, d.Round(time.Second), s.cfg.MaxClockSkew)
	}
	return now.UTC().Truncate(time.Second), nil
}

// recordTime applies the rule RecordLimit gives: the time handed to Add, Activate, Retire or Revoke is the
// earlier of the clock and the limit of the set, so an idle set is brought forward in steps.
func recordTime(set ledgerkeys.Set, now time.Time) time.Time {
	if lim := set.RecordLimit(); !lim.IsZero() && now.After(lim) {
		return lim.UTC().Truncate(time.Second)
	}
	return now
}

// origins are the key names a key will sign: the cluster origin (the key name of the events) and the origin
// of every log that exists. The key id of a new key is checked under each of them (AddChecked, correction 101).
func (s *Service) origins(ctx context.Context, prefix string) ([]string, error) {
	out := []string{prefix}
	logs, err := s.cfg.Store.LogOrigins(ctx)
	if err != nil {
		return nil, err
	}
	return append(out, logs...), nil
}

// ---------------------------------------------------------------------------------------------------------
// Writing: every change is made under the term and the revision.

// termError is the answer of a step that does not hold the term: it ends the run and says why.
type coded struct {
	code      string
	msg       string
	retryable bool
}

func (e *coded) Error() string       { return e.msg }
func (e *coded) TaskCode() string    { return e.code }
func (e *coded) TaskRetryable() bool { return e.retryable }

// waitTerm waits for the scheduler's term (a leader that has just been elected waits one lease margin).
func (s *Service) waitTerm(ctx context.Context) (scheduler.Term, error) {
	deadline := time.Now().Add(s.cfg.TermWait)
	for {
		if t, ok := s.cfg.Term(); ok {
			return t, nil
		}
		if time.Now().After(deadline) {
			return 0, &coded{code: "not_leader", msg: ErrNotLeader.Error(), retryable: true}
		}
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// mutate reads the state, lets f decide a change and writes it. f returns a nil change when there is
// nothing to do (the step was done already). A stored set that is damaged refuses every change.
func (s *Service) mutate(ctx context.Context, f func(v *view, now time.Time) (*Change, error)) error {
	var lastErr error
	for try := 0; try < s.cfg.ConflictTries; try++ {
		term, err := s.waitTerm(ctx)
		if err != nil {
			return err
		}
		v, err := s.load(ctx)
		if err != nil {
			return err
		}
		if v.alarm != nil {
			return &coded{code: "key_set_damaged", msg: v.alarm.Error()}
		}
		now, err := s.clock(ctx)
		if err != nil {
			return &coded{code: "clock", msg: err.Error(), retryable: true}
		}
		ch, err := f(v, now)
		if err != nil {
			return err
		}
		if ch == nil {
			return nil
		}
		if len(ch.Events) > 0 && s.cfg.PublishedCheck != nil {
			if err := s.cfg.PublishedCheck(ctx); err != nil {
				return &coded{code: "published_state", msg: err.Error()}
			}
		}
		if len(ch.Events) > 0 && s.cfg.Published != nil {
			// an event is appended only while the database holds what was published (P8-01): a restored database would reuse numbers
			var last []byte
			if n := len(v.snap.Events); n > 0 {
				last = v.snap.Events[n-1].Note
			}
			if err := s.cfg.Published.AllowEvent(int64(len(v.snap.Events)), last); err != nil {
				return &coded{code: "published_state", msg: err.Error()}
			}
		}
		switch err := s.cfg.Store.Commit(ctx, term, v.snap.Revision, *ch); {
		case err == nil:
			if s.cfg.Published != nil {
				var werr error
				for _, e := range ch.Events {
					if err := s.cfg.Published.RecordEvent(e.Seq, e.Note); err != nil {
						if s.cfg.Log != nil {
							s.cfg.Log.Error("an event was stored and could not be noted in the record of what was published", "seq", e.Seq, "error", err.Error())
						}
						werr = err
					}
				}
				if werr != nil {
					// The event is stored; the record, which guards every later signature, could not be written (a full disk is not a crash). The
					// task that made the event fails and says so, and nothing is signed until the record is written again (P7A-11).
					return &coded{code: "published_state_write", msg: "the event was stored, but the record of what was published could not be written: " + werr.Error(), retryable: true}
				}
			}
			return nil
		case errors.Is(err, ErrConflict):
			lastErr = err
			continue
		case errors.Is(err, scheduler.ErrDeposed):
			return &coded{code: "term_over", msg: scheduler.ErrDeposed.Error(), retryable: true}
		default:
			return err
		}
	}
	return fmt.Errorf("ledgersign: the stored state kept changing: %w", lastErr)
}

// rows merges the rows the store has with the set after a transition.
func rows(old []KeyRow, set ledgerkeys.Set, task int64, only string) []KeyRow {
	by := map[string]KeyRow{}
	for _, r := range old {
		by[r.Fingerprint()] = r
	}
	var out []KeyRow
	for _, k := range set {
		if only != "" && k.Fingerprint() != only {
			continue
		}
		r := by[k.Fingerprint()]
		r.Key = k
		if r.Mode == "" {
			r.Mode = "file"
		}
		if r.TaskID == 0 {
			r.TaskID = task
		}
		out = append(out, r)
	}
	return out
}

func (s *Service) trustFor(ctx context.Context, prefix string, set ledgerkeys.Set) ([]byte, error) {
	origins, err := s.origins(ctx, prefix)
	if err != nil {
		return nil, err
	}
	return ledgerkeys.TrustFile(prefix, set, s.trustOptions(), origins[1:]...)
}

func (s *Service) eventExists(v *view, typ, fp string) (EventRow, bool) {
	for _, e := range v.snap.Events {
		if e.Type == typ && e.Subject == fp {
			return e, true
		}
	}
	return EventRow{}, false
}

// signEvent signs the text of an event under the cluster origin with the keys that may sign it, each
// checked with CanSign (the purpose of the event and the subject).
func (s *Service) signEvent(ctx context.Context, v *view, text []byte, purpose ledgerkeys.Purpose, subject string, signers []ledgerkeys.Key, now time.Time) ([]byte, error) {
	var ss []ledgerkeys.Signer
	for _, k := range signers {
		if err := v.set.CanSign(k.Fingerprint(), purpose, subject, now); err != nil {
			return nil, &coded{code: "cannot_sign", msg: err.Error()}
		}
		sg, err := s.cfg.Keyring.Signer(k.Fingerprint())
		if err != nil {
			return nil, &coded{code: "signer_unavailable", msg: fmt.Sprintf("the key %s cannot be opened on this server: %v", k.Fingerprint()[:16], err), retryable: true}
		}
		if !bytes.Equal(sg.Public(), k.Public) {
			return nil, &coded{code: "signer_unavailable", msg: "the key file of " + k.Fingerprint()[:16] + " holds another key"}
		}
		ss = append(ss, sg)
	}
	n, err := ledgerlogSign(ctx, text, v.prefix, ss)
	if err != nil {
		return nil, &coded{code: "sign_failed", msg: err.Error()}
	}
	return n, nil
}

// activeSigners are the keys that are active and in force at now, other than the ones named.
func activeSigners(set ledgerkeys.Set, now time.Time, except ...string) []ledgerkeys.Key {
	keys, err := set.Signers(now)
	if err != nil {
		return nil
	}
	var out []ledgerkeys.Key
	for _, k := range keys {
		skip := false
		for _, e := range except {
			if k.Fingerprint() == e {
				skip = true
			}
		}
		if !skip {
			out = append(out, k)
		}
	}
	return out
}

func appendEvent(v *view, typ, subject string, at time.Time, fields []field, sign func(text []byte) ([]byte, error)) (EventRow, error) {
	seq := int64(len(v.snap.Events))
	text, err := eventText(seq, at, typ, fields...)
	if err != nil {
		return EventRow{}, err
	}
	note, err := sign(text)
	if err != nil {
		return EventRow{}, err
	}
	return EventRow{Seq: seq, Type: typ, Subject: subject, Note: note, CreatedAt: at}, nil
}

func b64(pub []byte) string { return base64.StdEncoding.EncodeToString(pub) }

// ---------------------------------------------------------------------------------------------------------
// Reading: the operations getLedgerSigning, getLedgerTrustFile, getLedgerSettings.

func timePtr(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	u := t.UTC()
	return &u
}

// Signing is the answer of getLedgerSigning. It holds no secret.
func (s *Service) Signing(ctx context.Context) (api.LedgerSigning, error) {
	v, err := s.load(ctx)
	if err != nil {
		return api.LedgerSigning{}, err
	}
	out := api.LedgerSigning{Mode: "file", Keys: []api.LedgerSigningKey{}, Events: []api.LedgerSigningEvent{}, Holders: []string{},
		BackupAdvised: v.snap.Cluster.BackupAdvised, Discontinuity: v.snap.Cluster.Discontinuity}
	if id := v.snap.Cluster.ClusterID; len(id) == 32 {
		h := fmt.Sprintf("%x", id)
		out.ClusterID = &h
	}
	if prot, err := s.cfg.Keyring.Protected(); err == nil {
		out.PassphraseProtected = prot
	}
	out.Holders = append(out.Holders, v.snap.Cluster.Holders...)
	sort.Strings(out.Holders)
	for _, k := range v.set {
		ak := api.LedgerSigningKey{Fingerprint: k.Fingerprint(), PublicKey: b64(k.Public), State: k.State.String(), CreatedAt: k.CreatedAt.UTC(),
			ActivatedAt: timePtr(k.ActivatedAt), RetiredAt: timePtr(k.RetiredAt), RevokedAt: timePtr(k.RevokedAt), CompromisedSince: timePtr(k.CompromisedSince)}
		if k.MaxTreeSize != 0 {
			m := int64(k.MaxTreeSize) //nolint:gosec // at most 2^62-1
			ak.MaxTreeSize = &m
		}
		out.Keys = append(out.Keys, ak)
	}
	for _, e := range v.snap.Events {
		out.Events = append(out.Events, api.LedgerSigningEvent{Seq: e.Seq, Type: e.Type, Time: e.CreatedAt.UTC()})
	}
	if a := s.alarmText(v); a != "" {
		out.SignerAlarm = &a
	}
	return out, nil
}

// alarmText is why this server does not sign, "" when it can. It names no secret.
func (s *Service) alarmText(v *view) string {
	if v.alarm != nil {
		return v.alarm.Error()
	}
	if len(v.set) == 0 {
		return ""
	}
	for _, k := range v.set {
		if (k.State == ledgerkeys.StateActive || k.State == ledgerkeys.StateNext) && !s.cfg.Keyring.Has(k.Fingerprint()) {
			return fmt.Sprintf("the key file of the %s key %s is missing on this server", k.State, k.Fingerprint()[:16])
		}
		if k.State == ledgerkeys.StateActive || k.State == ledgerkeys.StateNext {
			// a file that exists and does not open (garbage, another passphrase, another key under this name) is as bad as a missing one (P7-04)
			if err := s.cfg.Keyring.Verify(k.Fingerprint(), k.Public); err != nil {
				return fmt.Sprintf("the key file of the %s key %s cannot be used on this server: %s", k.State, k.Fingerprint()[:16], err.Error())
			}
		}
	}
	return ""
}

// Alarm is the reason this server cannot sign, "" when it can or no key exists.
func (s *Service) Alarm(ctx context.Context) (string, error) {
	v, err := s.load(ctx)
	if err != nil {
		return "", err
	}
	return s.alarmText(v), nil
}

// TrustFile returns the trust file of the verifier: the one stored at the last change, after it was
// checked against the key set. 409 signing_key_missing when no key exists.
func (s *Service) TrustFile(ctx context.Context) ([]byte, error) {
	v, err := s.load(ctx)
	if err != nil {
		return nil, err
	}
	if !hasTrustedKey(v.set) {
		return nil, problem.New(problem.SigningKeyMissing, "No signing key has been created yet.")
	}
	if v.alarm != nil {
		return nil, problem.New(problem.ServiceUnavailable, "The key set does not agree with the trust file that was published: no trust file is served until an operator has looked at it ("+v.alarm.Error()+").")
	}
	return append([]byte(nil), v.snap.Cluster.TrustFile...), nil
}

// Settings is the answer of getLedgerSettings.
func (s *Service) Settings(ctx context.Context) (api.LedgerSettingsCluster, error) {
	snap, err := s.cfg.Store.Load(ctx)
	if err != nil {
		return api.LedgerSettingsCluster{}, err
	}
	return s.settingsOf(snap), nil
}

func (s *Service) settingsOf(snap Snapshot) api.LedgerSettingsCluster {
	c := snap.Cluster
	return api.LedgerSettingsCluster{
		Publication: api.LedgerPublicationSettings{Enabled: c.PublicationEnabled, OriginPrefix: s.prefixOf(c), PublicURL: c.PublicURL,
			IntervalSeconds: c.PublicationInterval, OriginPrefixLocked: c.FirstCheckpointAt != nil || len(snap.Events) > 0},
		Anchor: api.LedgerAnchorSettings{IntervalSeconds: c.AnchorInterval, MinimumAnchored: c.AnchorMinimum, Required: c.AnchorRequired},
	}
}

func fieldErr(f, msg, code string) problem.FieldError {
	return problem.FieldError{Field: f, Message: msg, Code: code}
}

// UpdateSettings is updateLedgerSettings. The origin prefix is locked once the first checkpoint exists or the
// first key was made (correction 124: the key-added event is signed under the prefix, and a signed event
// cannot be signed again under another name).
func (s *Service) UpdateSettings(ctx context.Context, in api.LedgerSettingsUpdate) (api.LedgerSettingsCluster, error) {
	var out api.LedgerSettingsCluster
	var problemOut error
	err := func() error {
		var lastErr error
		for try := 0; try < s.cfg.ConflictTries; try++ {
			term, ok := s.cfg.Term()
			if !ok {
				return problem.New(problem.ServiceUnavailable, "The manager has just started; try again in a few seconds.").WithRetryAfter(5)
			}
			snap, err := s.cfg.Store.Load(ctx)
			if err != nil {
				return err
			}
			c := snap.Cluster
			var errs []problem.FieldError
			if p := in.Publication; p != nil {
				if p.Enabled != nil {
					c.PublicationEnabled = *p.Enabled
				}
				if p.IntervalSeconds != nil {
					if *p.IntervalSeconds < 10 || *p.IntervalSeconds > 3600 {
						errs = append(errs, fieldErr("/publication/intervalSeconds", "must be 10 to 3600", "out_of_range"))
					}
					c.PublicationInterval = *p.IntervalSeconds
				}
				if p.PublicURL.Set {
					if p.PublicURL.Value != nil && (len(*p.PublicURL.Value) > 1024 || !printableASCII(*p.PublicURL.Value)) {
						errs = append(errs, fieldErr("/publication/publicUrl", "must be at most 1024 characters of printable ASCII", "pattern"))
					}
					c.PublicURL = p.PublicURL.Value
				}
				if p.OriginPrefix != nil && *p.OriginPrefix != s.prefixOf(snap.Cluster) {
					if err := ledgerkeys.ValidPrefix(*p.OriginPrefix); err != nil {
						errs = append(errs, fieldErr("/publication/originPrefix", err.Error(), "pattern"))
					} else if c.FirstCheckpointAt != nil || len(snap.Events) > 0 {
						problemOut = problem.New(problem.OriginPrefixLocked, "The origin prefix is the name under which every signature is made and cannot change once the first key or checkpoint exists. "+
							"A new prefix needs a new generation of the trust file (the procedure of the design, 9.4).")
						return nil
					} else {
						c.OriginPrefix = *p.OriginPrefix
					}
				}
			}
			if a := in.Anchor; a != nil {
				if a.IntervalSeconds != nil {
					if *a.IntervalSeconds < 60 || *a.IntervalSeconds > 86400 {
						errs = append(errs, fieldErr("/anchor/intervalSeconds", "must be 60 to 86400", "out_of_range"))
					}
					c.AnchorInterval = *a.IntervalSeconds
				}
				if a.MinimumAnchored != nil {
					if *a.MinimumAnchored < 1 || *a.MinimumAnchored > 8 {
						errs = append(errs, fieldErr("/anchor/minimumAnchored", "must be 1 to 8", "out_of_range"))
					}
					c.AnchorMinimum = *a.MinimumAnchored
				}
				if a.Required != nil {
					c.AnchorRequired = *a.Required
				}
			}
			if len(errs) > 0 {
				problemOut = problem.Validation("One or more settings are invalid.", errs...)
				return nil
			}
			ch := Change{Cluster: &c}
			switch err := s.cfg.Store.Commit(ctx, term, snap.Revision, ch); {
			case err == nil:
				snap.Cluster = c
				out = s.settingsOf(snap)
				return nil
			case errors.Is(err, ErrConflict):
				lastErr = err
			case errors.Is(err, scheduler.ErrDeposed):
				return problem.New(problem.ServiceUnavailable, "This server is no longer the manager leader.").WithRetryAfter(5)
			default:
				return err
			}
		}
		return fmt.Errorf("ledgersign: the settings kept changing: %w", lastErr)
	}()
	if err != nil {
		return out, err
	}
	return out, problemOut
}

// newClusterID makes the public identifier of the cluster (3.2.1).
func newClusterID() ([]byte, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	return b, nil
}

// Holders returns this server and the servers that run an instance of shard group 0, sorted, without
// repeats.
func (s *Service) holderSet(ctx context.Context) ([]string, error) {
	seen := map[string]bool{s.cfg.Self: true}
	if s.cfg.Holders != nil {
		hs, err := s.cfg.Holders(ctx)
		if err != nil {
			return nil, err
		}
		for _, h := range hs {
			seen[h] = true
		}
	}
	out := make([]string, 0, len(seen))
	for h := range seen {
		out = append(out, h)
	}
	sort.Strings(out)
	return out, nil
}

func fingerprintOK(fp string) bool {
	return fingerprintRE.MatchString(strings.ToLower(fp)) && fp == strings.ToLower(fp)
}

// Published is what the key service needs of the state of internal/ledgerstate.
type Published interface {
	AllowEvent(count int64, lastNote []byte) error
	RecordEvent(seq int64, note []byte) error
}
