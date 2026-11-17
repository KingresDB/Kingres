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
	// MaxUpgradeLookups bounds the GET requests of one Upgrade call.
	MaxUpgradeLookups = 32
	// FirstCheck is the delay before the first upgrade attempt.
	FirstCheck = 10 * time.Minute
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
}

// Target is the OpenTimestamps target. It implements anchor.Target and is
// safe for concurrent use.
type Target struct {
	cfg  Config
	cal  *CalendarClient
	now  anchor.Clock
	rand io.Reader
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
	return &Target{cfg: n, cal: NewCalendarClient(opt.Client, opt.UserAgent, now), now: now, rand: rd}, nil
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
	digest := p.Digest[:]
	have := map[string][]byte{}
	if prior != nil {
		if prior.Digest != hex.EncodeToString(digest) {
			return anchor.Receipt{}, anchor.Errorf(anchor.ClassPermanent, anchor.KindOTS, "submit", "prior_digest", "prior evidence is for another digest")
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

	ev := anchor.OTSEvidence{Digest: hex.EncodeToString(digest), Calendars: []anchor.OTSCalendar{}}
	answered, retryable := 0, 0
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
		if anchor.IsRetryable(r.err) {
			retryable++
		}
	}
	if answered < t.cfg.MinCalendars {
		class := anchor.ClassRetryable
		if answered+retryable < t.cfg.MinCalendars {
			class = anchor.ClassPermanent
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

// Upgrade implements anchor.Target: it asks the calendars named by the
// pending attestations of the receipt's fragments (only calendars of the
// configuration; one outside it is never asked) for the Bitcoin attestation.
// A calendar's 404 means "not yet" and is not an error. When some fragment
// holds a Bitcoin attestation the receipt is "anchored" and EvidenceTime is
// nil (the time is the header's, known to the verifier). An error is
// returned only when every lookup failed.
func (t *Target) Upgrade(ctx context.Context, r anchor.Receipt) (anchor.Receipt, error) {
	if r.State == anchor.ReceiptAnchored {
		return r, nil
	}
	ev, err := anchor.ParseOTSEvidence(r.Evidence)
	if err != nil {
		return r, anchor.Errorf(anchor.ClassPermanent, anchor.KindOTS, "upgrade", "evidence", "the evidence is not readable")
	}
	digest, _ := hex.DecodeString(ev.Digest)
	allowed := map[string]bool{}
	for _, c := range t.cfg.Calendars {
		allowed[c] = true
	}
	roots := make([]*Node, len(ev.Calendars))
	for i, c := range ev.Calendars {
		f, _ := base64.StdEncoding.DecodeString(c.FragmentB64)
		n, err := ParseFragment(f, digest)
		if err != nil {
			return r, anchor.Errorf(anchor.ClassPermanent, anchor.KindOTS, "upgrade", "evidence", "a stored fragment is not readable")
		}
		roots[i] = n
	}
	type look struct {
		node *Node
		att  Attestation
		base string
	}
	var looks []look
	for _, root := range roots {
		root.Walk(func(n *Node) {
			for _, a := range n.Attestations {
				if a.Kind != AttPending || len(looks) >= MaxUpgradeLookups {
					continue
				}
				if base, err := NormalizeCalendarURL(a.URI, t.cfg.AllowInsecure); err == nil && allowed[base] {
					looks = append(looks, look{n, a, base})
				}
			}
		})
	}
	var lastErr error
	failed, ok := 0, 0
	changed := false
	for _, l := range looks {
		cctx, cancel := context.WithTimeout(ctx, CallTimeout)
		got, found, err := t.cal.GetTimestamp(cctx, l.base, l.node.Msg)
		cancel()
		if err != nil {
			failed++
			lastErr = err
			continue
		}
		ok++
		if !found || len(BitcoinClaims(got)) == 0 {
			continue
		}
		if err := l.node.Merge(got); err != nil {
			failed++
			lastErr = anchor.Errorf(anchor.ClassPermanent, anchor.KindOTS, "upgrade", "merge", "cannot merge the answer of a calendar: %s", shortErr(err))
			continue
		}
		removePending(l.node, l.att)
		changed = true
	}
	if failed > 0 && ok == 0 && !changed {
		return r, lastErr
	}
	next := t.now().Add(FirstCheck)
	if !changed && ev.Bitcoin == nil {
		out := r
		out.State = anchor.ReceiptSubmitted
		if out.NextCheck == nil {
			out.NextCheck = &next
		}
		return out, nil
	}
	cals := make([]anchor.OTSCalendar, 0, len(roots))
	btc := ev.Bitcoin
	for i, root := range roots {
		b, err := root.Serialize()
		if err != nil || len(b) > MaxFragment {
			return r, anchor.Errorf(anchor.ClassPermanent, anchor.KindOTS, "upgrade", "fragment", "the upgraded fragment is empty or larger than %d bytes", MaxFragment)
		}
		cals = append(cals, anchor.OTSCalendar{URI: ev.Calendars[i].URI, FragmentB64: base64.StdEncoding.EncodeToString(b)})
		if btc == nil {
			if claims := BitcoinClaims(root); len(claims) > 0 {
				min := claims[0].Att.Height
				for _, c := range claims {
					if c.Att.Height < min {
						min = c.Att.Height
					}
				}
				btc = &anchor.OTSBitcoin{Height: min, CalendarURI: ev.Calendars[i].URI}
			}
		}
	}
	ev.Calendars = cals
	ev.Bitcoin = btc
	raw, err := anchor.EncodeEvidence(ev)
	if err != nil {
		return r, anchor.Errorf(anchor.ClassPermanent, anchor.KindOTS, "upgrade", "evidence", "cannot encode the evidence")
	}
	if btc != nil {
		return anchor.Receipt{State: anchor.ReceiptAnchored, Evidence: raw}, nil
	}
	return anchor.Receipt{State: anchor.ReceiptSubmitted, Evidence: raw, NextCheck: &next}, nil
}

func removePending(n *Node, a Attestation) {
	out := n.Attestations[:0]
	for _, x := range n.Attestations {
		if compareAtt(x, a) != 0 {
			out = append(out, x)
		}
	}
	n.Attestations = out
}

// Expired reports whether a submitted anchor created at createdAt has waited
// for a Bitcoin attestation longer than timeoutDays (0 means the default).
func Expired(createdAt, now time.Time, timeoutDays int) bool {
	if timeoutDays <= 0 {
		timeoutDays = DefaultUpgradeTimeoutDays
	}
	return !now.Before(createdAt.Add(time.Duration(timeoutDays) * 24 * time.Hour))
}

// BuildFile merges every calendar fragment of ev into one tree and returns
// the detached timestamp file for the digest (file hash operation SHA-256).
func BuildFile(ev anchor.OTSEvidence) (*File, error) {
	if err := ev.Validate(); err != nil {
		return nil, err
	}
	digest, _ := hex.DecodeString(ev.Digest)
	if len(ev.Calendars) == 0 {
		return nil, errors.New("ots: evidence holds no fragment")
	}
	var root *Node
	for _, c := range ev.Calendars {
		f, _ := base64.StdEncoding.DecodeString(c.FragmentB64)
		n, err := ParseFragment(f, digest)
		if err != nil {
			return nil, fmt.Errorf("ots: fragment of a calendar: %w", err)
		}
		if root == nil {
			root = n
			continue
		}
		if err := root.Merge(n); err != nil {
			return nil, err
		}
	}
	return &File{HashOp: TagSHA256, Root: root}, nil
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
