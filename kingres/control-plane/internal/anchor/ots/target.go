// SPDX-License-Identifier: AGPL-3.0-only

package ots

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/anchor"
)

// Defaults and limits of the configuration and of the exchange.
const (
	DefaultMinCalendars       = 2
	DefaultUpgradeTimeoutDays = 7
	MaxUpgradeTimeoutDays     = 365
	MaxCalendars              = anchor.MaxOTSCalendars
	// CallTimeout is the cap on one calendar request (design 6.5: 20 s).
	CallTimeout = 20 * time.Second
	// MaxParallel bounds the concurrent calendar requests of one call.
	MaxParallel = 8
	// MaxFragment is the largest stored fragment (a calendar answer is at most
	// MaxCalendarResponse; upgrades merge more in).
	MaxFragment = 64 << 10
	// PerFragmentLookups is how many GET requests one calendar's fragment may
	// cause in one Upgrade call (distinct commitments, in tree order). The
	// budget is per fragment, so a calendar that fills its fragment with
	// pending attestations spends only its own share.
	PerFragmentLookups = 4
	// MaxUpgradeLookups bounds the GET requests of one Upgrade call.
	MaxUpgradeLookups = MaxCalendars * PerFragmentLookups
	// MaxHeaderChecks bounds the headers asked of a HeaderSource in one call.
	MaxHeaderChecks = 8
	// FirstCheck is the delay before the first upgrade attempt.
	FirstCheck = 10 * time.Minute
	// DefaultUpgradeTimeout is the bound of one Upgrade call (Options.UpgradeTimeout):
	// the worst round (16 fragments of 4 lookups, 8 in parallel, 20 s each, and 8
	// header requests of 20 s) would take about 5 minutes.
	DefaultUpgradeTimeout = 5 * time.Minute
	// DefaultContradictionConfirmMinutes is how long after the first
	// contradiction of a claim by a header the header source has to contradict
	// it again before the claim is dropped for good.
	DefaultContradictionConfirmMinutes = 60
	// MaxContradictionConfirmMinutes is the largest Config.ContradictionConfirmMinutes
	// (7 days).
	MaxContradictionConfirmMinutes = 7 * 24 * 60
)

// Config is the non-secret configuration of an OpenTimestamps target (6.5).
type Config struct {
	// Calendars are the base URLs of the calendars: https (http only with
	// AllowInsecure), no user information, no query.
	Calendars []string `json:"calendars"`
	// MinCalendars is how many calendars must have answered before the anchor
	// is accepted. 0 means the default: 2, or the number of calendars if smaller.
	MinCalendars int `json:"minCalendars,omitempty"`
	// UpgradeTimeoutDays is how long a submitted anchor waits for a Bitcoin
	// attestation. 0 means 7.
	UpgradeTimeoutDays int `json:"upgradeTimeoutDays,omitempty"`
	// AllowInsecure permits http calendars (tests and private networks only).
	AllowInsecure bool `json:"allowInsecure,omitempty"`
	// ContradictionConfirmMinutes is how long after a block header first
	// contradicted a claim of a calendar the header source has to contradict it
	// again, in a later round, before the claim is dropped for good (until then
	// it is kept aside and the calendar is asked again). 0 means 60; the range
	// is 1 to 10080.
	ContradictionConfirmMinutes int `json:"contradictionConfirmMinutes,omitempty"`
}

// ParseConfig parses, validates and normalizes a configuration: unknown
// members are rejected, URLs normalized and deduplicated, defaults applied.
func ParseConfig(raw json.RawMessage) (Config, error) {
	var c Config
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return Config{}, fmt.Errorf("ots: configuration: %w", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return Config{}, errors.New("ots: configuration: trailing data")
	}
	return c.Normalize()
}

// Normalize validates c and returns it with defaults applied.
func (c Config) Normalize() (Config, error) {
	if len(c.Calendars) == 0 {
		return Config{}, errors.New("ots: configuration: at least one calendar is required")
	}
	if len(c.Calendars) > MaxCalendars {
		return Config{}, fmt.Errorf("ots: configuration: at most %d calendars", MaxCalendars)
	}
	out := Config{AllowInsecure: c.AllowInsecure}
	seen := map[string]bool{}
	for i, u := range c.Calendars {
		n, err := NormalizeCalendarURL(u, c.AllowInsecure)
		if err != nil {
			return Config{}, fmt.Errorf("ots: configuration: calendar %d: %v", i+1, err)
		}
		if seen[n] {
			return Config{}, fmt.Errorf("ots: configuration: calendar %d is a repeat", i+1)
		}
		seen[n] = true
		out.Calendars = append(out.Calendars, n)
	}
	out.MinCalendars = c.MinCalendars
	if out.MinCalendars == 0 {
		out.MinCalendars = DefaultMinCalendars
		if len(out.Calendars) < out.MinCalendars {
			out.MinCalendars = len(out.Calendars)
		}
	}
	if out.MinCalendars < 1 || out.MinCalendars > len(out.Calendars) {
		return Config{}, errors.New("ots: configuration: minCalendars must be between 1 and the number of calendars")
	}
	out.ContradictionConfirmMinutes = c.ContradictionConfirmMinutes
	if out.ContradictionConfirmMinutes == 0 {
		out.ContradictionConfirmMinutes = DefaultContradictionConfirmMinutes
	}
	if out.ContradictionConfirmMinutes < 1 || out.ContradictionConfirmMinutes > MaxContradictionConfirmMinutes {
		return Config{}, fmt.Errorf("ots: configuration: contradictionConfirmMinutes must be between 1 and %d", MaxContradictionConfirmMinutes)
	}
	out.UpgradeTimeoutDays = c.UpgradeTimeoutDays
	if out.UpgradeTimeoutDays == 0 {
		out.UpgradeTimeoutDays = DefaultUpgradeTimeoutDays
	}
	if out.UpgradeTimeoutDays < 1 || out.UpgradeTimeoutDays > MaxUpgradeTimeoutDays {
		return Config{}, fmt.Errorf("ots: configuration: upgradeTimeoutDays must be between 1 and %d", MaxUpgradeTimeoutDays)
	}
	return out, nil
}

// Options are the caller-supplied parts of a Target.
type Options struct {
	// Client is the HTTP client (nil: default transport).
	Client *http.Client
	// Clock gives the time (nil: time.Now).
	Clock anchor.Clock
	// Rand is the source of the random digests of Test (nil: crypto/rand).
	Rand io.Reader
	// UserAgent is sent to the calendars.
	UserAgent string
	// Headers, when not nil, is the operator's source of Bitcoin block
	// headers. With it a claim is checked against the header of its height
	// and the record is anchored only when the check passes; without it a
	// claim stays a claim (receipt state "claimed").
	Headers HeaderSource
	// UpgradeTimeout bounds one Upgrade call besides the caller's context (0
	// means DefaultUpgradeTimeout).
	UpgradeTimeout time.Duration
	// WorkSlack is how many times easier than the target of the nearest
	// earlier checkpoint of the table of real headers the target of an accepted
	// header may be (0 means DefaultWorkSlack, 16): the minimum-work rule of
	// header_checked, see CheckHeaderAt. A larger value accepts less work; it
	// exists for a chain whose difficulty has fallen by more than that. New
	// refuses a value above MaxWorkSlack (2^20), which can never admit
	// difficulty 1 for a height at or after the newest checkpoint.
	WorkSlack uint

	// headerWork replaces the whole proof-of-work check of a header (floor
	// included), and fragmentLimit the largest stored fragment: only the tests
	// of this package set them (the limits of the sizes keep a real call from
	// reaching a fragment past MaxFragment, and a header of the present era
	// cannot be mined in a test).
	headerWork    func([]byte) (HeaderResult, error)
	fragmentLimit int
}

// Target is the OpenTimestamps target. It implements anchor.Target and is
// safe for concurrent use.
type Target struct {
	cfg     Config
	cal     *CalendarClient
	now     anchor.Clock
	rand    io.Reader
	headers HeaderSource
	// work is the proof-of-work check of a header for the height of the claim
	// (CheckHeaderAt), fragLimit the largest stored fragment (MaxFragment);
	// see Options.
	work           func([]byte, uint64) (HeaderResult, error)
	fragLimit      int
	upgradeTimeout time.Duration
	confirm        time.Duration
}

var _ anchor.Target = (*Target)(nil)

// New returns a Target for a normalized or raw configuration.
func New(cfg Config, opt Options) (*Target, error) {
	n, err := cfg.Normalize()
	if err != nil {
		return nil, err
	}
	now := opt.Clock
	if now == nil {
		now = time.Now
	}
	rd := opt.Rand
	if rd == nil {
		rd = rand.Reader
	}
	slack := opt.WorkSlack
	if slack == 0 {
		slack = DefaultWorkSlack
	}
	if slack > MaxWorkSlack {
		return nil, fmt.Errorf("ots: WorkSlack must be at most %d: a larger value would admit headers of difficulty 1", MaxWorkSlack)
	}
	work := func(h []byte, height uint64) (HeaderResult, error) {
		return CheckHeaderFloor(h, MaxTargetAt(height, slack))
	}
	if hw := opt.headerWork; hw != nil {
		work = func(h []byte, _ uint64) (HeaderResult, error) { return hw(h) }
	}
	ut := opt.UpgradeTimeout
	if ut <= 0 {
		ut = DefaultUpgradeTimeout
	}
	limit := opt.fragmentLimit
	if limit <= 0 {
		limit = MaxFragment
	}
	return &Target{cfg: n, cal: NewCalendarClient(opt.Client, opt.UserAgent, now), now: now, rand: rd, headers: opt.Headers, work: work, fragLimit: limit,
		upgradeTimeout: ut, confirm: time.Duration(n.ContradictionConfirmMinutes) * time.Minute}, nil
}

// Config returns the normalized configuration.
func (t *Target) Config() Config { return t.cfg }

// Kind implements anchor.Target.
func (t *Target) Kind() string { return anchor.KindOTS }

// Validate implements anchor.Target: the shape of the configuration.
func (t *Target) Validate(cfg json.RawMessage) error {
	_, err := ParseConfig(cfg)
	return err
}

// PartialError is returned by Submit when fewer than MinCalendars calendars
// answered. Evidence holds what did answer (it may hold no calendar); the
// caller keeps it and passes it to SubmitWithPrior at the next attempt, so
// a calendar that answered is not asked again. It unwraps to an *anchor.Error.
type PartialError struct {
	Evidence anchor.OTSEvidence
	Err      *anchor.Error
}

// Error implements error.
func (e *PartialError) Error() string { return e.Err.Error() }

// Unwrap returns the *anchor.Error.
func (e *PartialError) Unwrap() error { return e.Err }

// ContradictedError is returned by Upgrade with Code "claim_contradicted" (class
// conflict): every claim of the record was dropped for good because the header
// source contradicted it twice, an hour or more apart, and nothing can deliver
// any more. Evidence is the evidence as it was BEFORE that call: it still holds
// the claims. If the operator finds that the header source was wrong (a node on
// a stale fork, still synchronising, of another network), the caller keeps this
// evidence with the failed record and passes it to SubmitWithPrior when the
// record is retried: the calendars are not asked again, the claims are judged
// again against the corrected source, and the submission time is kept. A Submit
// without it starts again from the calendars and the old proof is gone (correction
// 96). PriorEvidence reads it from any error of this package that carries one.
// It unwraps to an *anchor.Error.
type ContradictedError struct {
	Evidence anchor.OTSEvidence
	Err      *anchor.Error
}

// Error implements error.
func (e *ContradictedError) Error() string { return e.Err.Error() }

// Unwrap returns the *anchor.Error.
func (e *ContradictedError) Unwrap() error { return e.Err }

// PriorEvidence returns the evidence that a caller keeps and passes to
// SubmitWithPrior when it retries a record whose Submit or Upgrade failed with a
// *PartialError (what the calendars that did answer gave) or a *ContradictedError
// (what the record held before the claims were dropped). ok is false for any
// other error. A caller that retries such a record without it throws away
// what was paid for.
func PriorEvidence(err error) (ev anchor.OTSEvidence, ok bool) {
	var pe *PartialError
	if errors.As(err, &pe) {
		return pe.Evidence, true
	}
	var ce *ContradictedError
	if errors.As(err, &ce) {
		return ce.Evidence, true
	}
	return anchor.OTSEvidence{}, false
}

// Submit implements anchor.Target.
func (t *Target) Submit(ctx context.Context, p anchor.Payload) (anchor.Receipt, error) {
	return t.SubmitWithPrior(ctx, p, nil)
}

type calResult struct {
	uri  string
	frag []byte
	err  error
}

// SubmitWithPrior submits p.Digest to every configured calendar that is not
// already in prior (evidence kept from an earlier, partial attempt; its
// digest must equal p.Digest). The receipt is "submitted" when at least
// MinCalendars calendars have a fragment. Otherwise the error is a
// *PartialError.
func (t *Target) SubmitWithPrior(ctx context.Context, p anchor.Payload, prior *anchor.OTSEvidence) (anchor.Receipt, error) {
	if err := anchor.CheckPayload(anchor.KindOTS, p); err != nil {
		return anchor.Receipt{}, err
	}
	digest := p.Digest[:]
	have := map[string][]byte{}
	submittedAt := anchor.FormatTime(t.now())
	if prior != nil {
		if prior.Digest != hex.EncodeToString(digest) {
			return anchor.Receipt{}, anchor.Errorf(anchor.ClassPermanent, anchor.KindOTS, "submit", "prior_digest", "prior evidence is for another digest")
		}
		if _, err := anchor.ParseTime(prior.SubmittedAt); err == nil {
			submittedAt = prior.SubmittedAt
		}
		for _, c := range prior.Calendars {
			f, err := base64.StdEncoding.DecodeString(c.FragmentB64)
			if err != nil {
				return anchor.Receipt{}, anchor.Errorf(anchor.ClassPermanent, anchor.KindOTS, "submit", "prior_fragment", "prior evidence has a malformed fragment")
			}
			if _, err := ParseFragment(f, digest); err != nil {
				return anchor.Receipt{}, anchor.Errorf(anchor.ClassPermanent, anchor.KindOTS, "submit", "prior_fragment", "prior evidence has an unreadable fragment")
			}
			have[c.URI] = f
		}
	}
	results := make([]calResult, len(t.cfg.Calendars))
	sem := make(chan struct{}, MaxParallel)
	var wg sync.WaitGroup
	for i, base := range t.cfg.Calendars {
		results[i].uri = base
		if f, ok := have[base]; ok {
			results[i].frag = f
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, base string) {
			defer wg.Done()
			defer func() { <-sem }()
			cctx, cancel := context.WithTimeout(ctx, CallTimeout)
			defer cancel()
			n, err := t.cal.Submit(cctx, base, digest)
			if err != nil {
				results[i].err = err
				return
			}
			b, err := n.Serialize()
			if err != nil {
				results[i].err = anchor.Errorf(anchor.ClassPermanent, anchor.KindOTS, "submit", "serialize", "cannot serialize a fragment")
				return
			}
			results[i].frag = b
		}(i, base)
	}
	wg.Wait()

	ev := anchor.OTSEvidence{Digest: hex.EncodeToString(digest), SubmittedAt: submittedAt, Calendars: []anchor.OTSCalendar{}}
	answered, retryable, refused := 0, 0, 0
	var firstErr error
	for _, r := range results {
		if r.frag != nil {
			answered++
			ev.Calendars = append(ev.Calendars, anchor.OTSCalendar{URI: r.uri, FragmentB64: base64.StdEncoding.EncodeToString(r.frag)})
			continue
		}
		if firstErr == nil {
			firstErr = r.err
		}
		switch {
		case anchor.IsRetryable(r.err):
			retryable++
		case anchor.IsRefused(r.err):
			refused++
		}
	}
	if answered < t.cfg.MinCalendars {
		// A calendar that was only unreachable, slow or refused (401, 403) may
		// answer later: retryable, or refused (1, 5 and 15 minutes, then
		// failed) when the hope rests on a calendar that refused. Only when
		// the calendars that gave a wrong answer make the minimum out of
		// reach is it permanent.
		class := anchor.ClassPermanent
		switch {
		case answered+retryable >= t.cfg.MinCalendars:
			class = anchor.ClassRetryable
		case answered+retryable+refused >= t.cfg.MinCalendars:
			class = anchor.ClassRefused
		}
		e := &anchor.Error{Class: class, Target: anchor.KindOTS, Op: "submit", Code: "too_few_calendars",
			Text: fmt.Sprintf("%d of %d calendars answered, %d needed", answered, len(results), t.cfg.MinCalendars), Err: firstErr}
		e.RetryAfter = anchor.RetryAfterOf(firstErr)
		return anchor.Receipt{}, &PartialError{Evidence: ev, Err: e}
	}
	raw, err := anchor.EncodeEvidence(ev)
	if err != nil {
		return anchor.Receipt{}, anchor.Errorf(anchor.ClassPermanent, anchor.KindOTS, "submit", "evidence", "cannot encode the evidence")
	}
	next := t.now().Add(FirstCheck)
	return anchor.Receipt{State: anchor.ReceiptSubmitted, Evidence: raw, NextCheck: &next}, nil
}

// ClaimedTimeoutFactor is how many times longer than the timeout of a submitted
// record a claimed one is kept before it expires: it holds a claim that nothing
// checked, and it keeps asking the other calendars and the header source.
const ClaimedTimeoutFactor = 4

// Timeout is how long a submitted record waits for a Bitcoin attestation
// (timeoutDays 0 means the default); it is the timeout argument of
// anchor.Record.ExpireIfDue.
func Timeout(timeoutDays int) time.Duration {
	if timeoutDays <= 0 {
		timeoutDays = DefaultUpgradeTimeoutDays
	}
	return time.Duration(timeoutDays) * 24 * time.Hour
}

// ClaimedTimeout is how long a claimed record waits (ClaimedTimeoutFactor times
// Timeout); it is the claimedTimeout argument of anchor.Record.ExpireIfDue.
func ClaimedTimeout(timeoutDays int) time.Duration {
	return ClaimedTimeoutFactor * Timeout(timeoutDays)
}

// Expired reports whether a submitted anchor created at createdAt has waited
// for a Bitcoin attestation longer than timeoutDays (0 means the default).
func Expired(createdAt, now time.Time, timeoutDays int) bool {
	if timeoutDays <= 0 {
		timeoutDays = DefaultUpgradeTimeoutDays
	}
	return !now.Before(createdAt.Add(time.Duration(timeoutDays) * 24 * time.Hour))
}

// Export returns the bytes of the detached .ots file for the evidence:
// magic, version 1, SHA-256, the digest, the merged tree.
func Export(ev anchor.OTSEvidence) ([]byte, error) {
	f, err := BuildFile(ev)
	if err != nil {
		return nil, err
	}
	return f.Serialize()
}

// Test implements anchor.Target: a POST of 32 random bytes to each calendar
// (nothing is kept), one check per calendar and one for the minimum.
func (t *Target) Test(ctx context.Context) anchor.TestResult {
	var res anchor.TestResult
	errs := make([]error, len(t.cfg.Calendars))
	digests := make([][]byte, len(t.cfg.Calendars))
	for i := range digests {
		digests[i] = make([]byte, 32)
		if _, err := io.ReadFull(t.rand, digests[i]); err != nil {
			res.Add("random", anchor.CheckFailed, "no random bytes")
			return res
		}
	}
	var wg sync.WaitGroup
	sem := make(chan struct{}, MaxParallel)
	for i, base := range t.cfg.Calendars {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, base string) {
			defer wg.Done()
			defer func() { <-sem }()
			cctx, cancel := context.WithTimeout(ctx, CallTimeout)
			defer cancel()
			_, errs[i] = t.cal.Submit(cctx, base, digests[i])
		}(i, base)
	}
	wg.Wait()
	good := 0
	for i, base := range t.cfg.Calendars {
		name := "calendar " + hostOf(base)
		if errs[i] != nil {
			res.Add(name, anchor.CheckFailed, "%s", errs[i].Error())
			continue
		}
		good++
		res.Add(name, anchor.CheckOK, "answered with a pending timestamp")
	}
	if good >= t.cfg.MinCalendars {
		res.Add("minimum", anchor.CheckOK, "%d of %d calendars answered, %d needed", good, len(t.cfg.Calendars), t.cfg.MinCalendars)
	} else {
		res.Add("minimum", anchor.CheckFailed, "%d of %d calendars answered, %d needed", good, len(t.cfg.Calendars), t.cfg.MinCalendars)
	}
	return res
}
