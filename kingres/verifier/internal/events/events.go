// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

// Package events reads the signed events of a publication (ledger-phase7.md 3.2.4, 3.3, 5.5 and
// corrections 121 to 149): `events/<6 digits>`, numbered from 0 without a gap, each a signed
// note whose key name is the origin prefix of the cluster. It is written from the text of the
// design. The text of an event is printable ASCII:
//
//	kingres-ledger-event/1
//	seq: <decimal, the number of the file>
//	time: <RFC 3339 UTC, whole seconds>
//	type: <key-added | key-retired | key-revoked | chain-retired | ...>
//	<name: value lines of the type>
//
// Follow walks the events in order from the keys the auditor pinned. An event counts only when
// it is signed by a key that is authorised at that point: a pinned key that the trust file does
// not call revoked, or a key that an earlier event added, in each case until its own retirement
// or revocation event. A key that is not authorised signs nothing that counts, so a publisher
// cannot add a key, retire a chain or revoke a key by signing with a key the auditor does not
// trust.
package events

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"strconv"
	"strings"
	"time"

	"github.com/KingresDB/Kingres/kingres/verifier/internal/edkey"
	"github.com/KingresDB/Kingres/kingres/verifier/internal/safefile"
	"github.com/KingresDB/Kingres/kingres/verifier/internal/signednote"
	"github.com/KingresDB/Kingres/kingres/verifier/internal/tlog"
	"github.com/KingresDB/Kingres/kingres/verifier/internal/trust"
)

// Types the verifier gives a meaning to.
const (
	TypeKeyAdded     = "key-added"
	TypeKeyRetired   = "key-retired"
	TypeKeyRevoked   = "key-revoked"
	TypeChainRetired = "chain-retired"
)

// Limits.
const (
	// MaxEvents is the most events one walk reads.
	MaxEvents = 4096
	// MaxFields is the most lines of an event after the four fixed ones.
	MaxFields = 16
	// MaxReason is the longest free text of an event.
	MaxReason = 200
)

const header = "kingres-ledger-event/1"

// Event is a parsed event text.
type Event struct {
	Seq    uint64
	Time   time.Time
	Type   string
	Fields map[string]string
	order  []string
}

// Error is a refused event: the number and what is wrong.
type Error struct {
	Seq uint64
	Err error
}

func (e *Error) Error() string { return fmt.Sprintf("event %06d: %v", e.Seq, e.Err) }
func (e *Error) Unwrap() error { return e.Err }

func printable(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] > 0x7e {
			return false
		}
	}
	return true
}

// Parse reads the text of an event. It does not verify anything: the text of a note is not to be
// believed before a signature of a trusted key verified it.
func Parse(text []byte) (*Event, error) {
	if !printable(strings.ReplaceAll(string(text), "\n", "")) {
		return nil, errors.New("the text of an event is printable ASCII, and this one is not")
	}
	body, ok := strings.CutSuffix(string(text), "\n")
	if !ok {
		return nil, errors.New("the text of an event ends with a newline")
	}
	lines := strings.Split(body, "\n")
	if len(lines) < 4 || len(lines) > 4+MaxFields {
		return nil, fmt.Errorf("an event has 4 to %d lines, this one has %d", 4+MaxFields, len(lines))
	}
	if lines[0] != header {
		return nil, fmt.Errorf("the first line is not %q", header)
	}
	ev := &Event{Fields: map[string]string{}}
	for i, l := range lines[1:] {
		name, value, ok := strings.Cut(l, ": ")
		if !ok || name == "" || value == "" || strings.ContainsAny(name, ": ") {
			return nil, fmt.Errorf("line %d is not a line of the form name: value", i+2)
		}
		if _, dup := ev.Fields[name]; dup {
			return nil, fmt.Errorf("the line %q appears twice", name)
		}
		ev.Fields[name] = value
		ev.order = append(ev.order, name)
	}
	if len(ev.order) < 3 || ev.order[0] != "seq" || ev.order[1] != "time" || ev.order[2] != "type" {
		return nil, errors.New("the lines after the first are seq, time and type, in that order")
	}
	seq, err := canonicalUint(ev.Fields["seq"], 1<<31)
	if err != nil {
		return nil, fmt.Errorf("seq: %w", err)
	}
	ev.Seq = seq
	if ev.Time, err = utcTime(ev.Fields["time"]); err != nil {
		return nil, fmt.Errorf("time: %w", err)
	}
	ev.Type = ev.Fields["type"]
	for _, c := range ev.Type {
		if !(c == '-' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9') {
			return nil, fmt.Errorf("type %q is not a lower case name", ev.Type)
		}
	}
	return ev, nil
}

func canonicalUint(s string, max uint64) (uint64, error) {
	if s == "" || len(s) > 19 || (len(s) > 1 && s[0] == '0') {
		return 0, fmt.Errorf("%q is not a canonical decimal", s)
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, fmt.Errorf("%q is not a canonical decimal", s)
		}
	}
	n, err := strconv.ParseUint(s, 10, 64)
	if err != nil || n >= max {
		return 0, fmt.Errorf("%q is out of range", s)
	}
	return n, nil
}

func utcTime(s string) (time.Time, error) {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil || !strings.HasSuffix(s, "Z") || strings.Contains(s, ".") {
		return time.Time{}, fmt.Errorf("%q is not an RFC 3339 time in UTC with whole seconds, ending in Z", s)
	}
	return t.UTC(), nil
}

func hex32(s string) ([32]byte, error) {
	var out [32]byte
	if len(s) != 64 {
		return out, fmt.Errorf("%q is not 64 lower case hex characters", s)
	}
	b, err := hex.DecodeString(s)
	if err != nil || strings.ToLower(s) != s {
		return out, fmt.Errorf("%q is not 64 lower case hex characters", s)
	}
	copy(out[:], b)
	return out, nil
}

// Key is the state of one key of the walk.
type Key struct {
	Name        string
	Public      ed25519.PublicKey
	Fingerprint string
	// Pinned: the key is in the trust file the walk started from. Otherwise AddedBy is the number of
	// the event that added it.
	Pinned  bool
	AddedBy uint64
	// NotBefore is the time of the key-added event for a key it added, else the one of the file.
	NotBefore *time.Time
	NotAfter  *time.Time
	// Retired: the key is limited to MaxTreeSize (nil: unbounded). RetiredByEvent says that a
	// key-retired event of this walk did it (RetiredBy is its number); a key the file calls retired
	// has no event number known, so it keeps authorising events.
	Retired        bool
	RetiredByEvent bool
	RetiredBy      uint64
	MaxTreeSize    *uint64
	// Revoked: by the trust file (RevokedInFile) or by the key-revoked event number RevokedBy.
	Revoked          bool
	RevokedInFile    bool
	RevokedBy        uint64
	CompromisedSince *time.Time
	RevokeReason     string
}

// ChainRetired is a verified chain-retired event.
type ChainRetired struct {
	Seq            uint64
	Chain          [32]byte
	Epoch          uint32
	FinalHeight    uint64
	FinalBlockHash [32]byte
	Reason         string
	SignedBy       []string // fingerprints of the authorised keys that signed
}

// authError is an event that is well formed and signed by no key that counts at its place. A strict
// walk (trust follow) stops at it; a tolerant one (the checks of an aggregate) skips the event,
// which then changes no key and retires no chain, and records it.
type authError struct{ msg string }

func (e *authError) Error() string { return e.msg }

func noAuth(format string, args ...any) error { return &authError{fmt.Sprintf(format, args...)} }

// Skipped is an event that a tolerant walk did not apply.
type Skipped struct {
	Seq    uint64
	Type   string
	Reason string
}

// Walk is the result of Follow.
type Walk struct {
	Skipped []Skipped
	// UnauthorisedRetired names the chains that a skipped chain-retired event is about (the lines are not
	// trusted: this only lets a caller say that such an event exists).
	UnauthorisedRetired map[string]uint64
	Keys                []*Key
	Retired             map[string]ChainRetired // by ChainKey
	// EpochClosed are the verified epoch-closed events, by ChainKey of the chain and the epoch they closed.
	EpochClosed map[string]EpochClosed
	// UnauthorisedEpochClosed names the epochs that a skipped epoch-closed event is about (the lines are not trusted).
	UnauthorisedEpochClosed map[string]uint64
	// Restores are the verified restore-acknowledged events, in the order of their numbers.
	Restores []RestoreAcknowledged
	// UnreadRestores are the restore-acknowledged events that an authorised key signed and that are not in the strict form: nothing of them is used.
	UnreadRestores []UnreadRestore
	Count          int            // events read
	Other          map[string]int // verified events of a type this version does not interpret
	Notes          []string
}

// ChainKey is the key of a chain and epoch in Walk.Retired.
func ChainKey(chain [32]byte, epoch uint32) string {
	return hex.EncodeToString(chain[:]) + ":" + strconv.FormatUint(uint64(epoch), 10)
}

// Reader returns the bytes of event seq, or an error that wraps fs.ErrNotExist when there is no such
// event (which ends the walk).
type Reader func(seq uint64) ([]byte, error)

// FromSource reads `events/<6 digits>` of a publication.
func FromSource(read func(rel string, limit int64) ([]byte, error)) Reader {
	return func(seq uint64) ([]byte, error) {
		return read(fmt.Sprintf("events/%06d", seq), signednote.MaxNoteSize)
	}
}

// Follow reads events 0, 1, 2 ... until one is absent, verifying each under the keys that are
// authorised at that point (see the package comment), and returns the keys and the chain-retired
// events that result. An event that is present and wrong ends the walk with an *Error that names it,
// whatever follows. A publisher that stops serving the events at some number cannot be told from
// one that has no more: the number of events read is in Count, and a caller that holds an earlier
// count (the trust file it made at the last follow) can say that events went missing.
func Follow(file *trust.File, read Reader, strict bool) (*Walk, error) {
	w := &Walk{Retired: map[string]ChainRetired{}, Other: map[string]int{}, UnauthorisedRetired: map[string]uint64{}, EpochClosed: map[string]EpochClosed{}, UnauthorisedEpochClosed: map[string]uint64{}}
	byFP := map[string]*Key{}
	for _, k := range file.Keys {
		if err := edkey.Check(k.Public); err != nil {
			return nil, fmt.Errorf("the pinned key %q: %w", k.Name, err)
		}
		kk := &Key{Name: k.Name, Public: k.Public, Fingerprint: k.Fingerprint, Pinned: true, NotBefore: k.NotBefore, NotAfter: k.NotAfter,
			MaxTreeSize: k.MaxTreeSize, Retired: k.MaxTreeSize != nil, Revoked: k.Revoked, RevokedInFile: k.Revoked}
		w.Keys = append(w.Keys, kk)
		byFP[kk.Fingerprint] = kk
	}
	prefix := file.OriginPrefix
	for seq := uint64(0); ; seq++ {
		if seq >= MaxEvents {
			return w, &Error{seq, fmt.Errorf("more than %d events: the walk stops here, it is not a publication to follow", MaxEvents)}
		}
		raw, err := read(seq)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return w, nil
			}
			return w, &Error{seq, err}
		}
		w.Count++
		if err := w.apply(prefix, seq, raw, byFP); err != nil {
			var ae *authError
			if !strict && errors.As(err, &ae) {
				w.skip(seq, raw, ae)
				continue
			}
			return w, &Error{seq, err}
		}
	}
}

// CheckGap compares the names of the files of the events directory (a listing the source gave) with the
// number of events the walk read. Events are numbered from 0 without a gap, so a file with a number at or
// above Count that the walk did not reach means that the event number Count is missing: one was withheld
// or removed, and what follows it could not be applied. The error names the first number that is missing and
// the first that is present beyond it. Names that are not six digits are not events and are ignored.
func (w *Walk) CheckGap(names []string) error {
	first := uint64(0)
	found := false
	for _, n := range names {
		if len(n) != 6 {
			continue
		}
		v, err := canonicalSix(n)
		if err != nil || v < uint64(w.Count) {
			continue
		}
		if !found || v < first {
			first, found = v, true
		}
	}
	if !found {
		return nil
	}
	return &Error{uint64(w.Count), fmt.Errorf("event %06d is absent and event %06d is present: events are numbered from 0 without a gap, so one was withheld or removed, and the events after it were not applied", w.Count, first)}
}

func canonicalSix(s string) (uint64, error) {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, errors.New("not digits")
		}
	}
	return strconv.ParseUint(s, 10, 64)
}

// authorised reports whether k's signature counts for the event with this number.
func (k *Key) authorised(seq uint64) bool {
	switch {
	case k.RevokedInFile:
		return false
	case k.Revoked && k.RevokedBy < seq:
		return false
	case k.RetiredByEvent && k.RetiredBy < seq:
		return false
	}
	return true
}

func (w *Walk) apply(prefix string, seq uint64, raw []byte, byFP map[string]*Key) error {
	n, err := signednote.Parse(raw)
	if err != nil {
		return fmt.Errorf("not a signed note: %w", err)
	}
	ev, err := Parse(n.Text)
	if err != nil {
		return err
	}
	if ev.Seq != seq {
		return fmt.Errorf("the event says it is number %d, the file is number %d", ev.Seq, seq)
	}
	// The keys the signatures of this note are tried against: every key of the walk (a signature
	// of a key that is no longer authorised may be present and must verify, it just does not
	// count), and for a key-added the key it adds, whose signature proves possession.
	var cand *Key
	if ev.Type == TypeKeyAdded {
		if cand, err = w.parseAdded(ev, byFP); err != nil {
			return err
		}
	}
	keys := make([]signednote.Key, 0, len(w.Keys)+1)
	for _, k := range w.Keys {
		sk, err := signednote.NewKey(prefix, k.Public)
		if err != nil {
			return err
		}
		keys = append(keys, sk)
	}
	if cand != nil && !cand.Pinned && byFP[cand.Fingerprint] == nil {
		sk, err := signednote.NewKey(prefix, cand.Public)
		if err != nil {
			return err
		}
		keys = append(keys, sk)
	}
	signers, err := signednote.Verify(n, keys)
	if err != nil {
		if errors.Is(err, signednote.ErrNoTrustedSignature) {
			return noAuth("the event is signed by no key that is authorised here (a pinned key, or a key an earlier event added): it is refused, whoever else signed it")
		}
		return err
	}
	var authorised []*Key // keys that count, each once
	possession := false
	seen := map[string]bool{}
	for _, sk := range signers {
		fp := trust.Fingerprint(sk.Public)
		if seen[fp] {
			continue
		}
		seen[fp] = true
		if cand != nil && fp == cand.Fingerprint {
			possession = true
		}
		if k := byFP[fp]; k != nil && k.authorised(seq) {
			authorised = append(authorised, k)
		}
	}
	others := func(except string) []*Key {
		var out []*Key
		for _, k := range authorised {
			if k.Fingerprint != except {
				out = append(out, k)
			}
		}
		return out
	}
	if len(authorised) == 0 && !(cand != nil && possession && cand.Pinned) {
		return noAuth("the event is signed by no key that counts here: the keys that signed it are revoked or retired at this point, or not authorised")
	}
	switch ev.Type {
	case TypeKeyAdded:
		return w.keyAdded(ev, cand, possession, authorised, byFP)
	case TypeKeyRetired:
		k, bound, err := w.keyOf(ev, byFP)
		if err != nil {
			return err
		}
		if k.Revoked {
			return errors.New("the key was revoked before: a revoked key is not retired")
		}
		if k.RetiredByEvent {
			return errors.New("the key was retired before")
		}
		if len(others(k.Fingerprint)) == 0 {
			return noAuth("the retirement of a key must be signed by another authorised key: a key does not retire itself")
		}
		if v := ev.Fields["max-tree-size"]; v != "unbounded" && bound == nil {
			return fmt.Errorf("max-tree-size %q is neither a size from 1 nor \"unbounded\"", v)
		}
		// A key the file already limits keeps the smaller of the two bounds: following the events again
		// over the file a follow wrote must not loosen it.
		switch {
		case k.Retired && k.MaxTreeSize != nil && (bound == nil || *k.MaxTreeSize < *bound):
			bound = k.MaxTreeSize
		}
		k.Retired, k.RetiredByEvent, k.RetiredBy, k.MaxTreeSize = true, true, seq, bound
		if bound == nil {
			w.Notes = append(w.Notes, fmt.Sprintf("event %06d retires key %s with no bound (\"unbounded\"): a retired key that leaks can sign any larger tree until it is revoked", seq, k.Fingerprint[:16]))
		}
	case TypeKeyRevoked:
		k, _, err := w.keyOf(ev, byFP)
		if err != nil {
			return err
		}
		if k.Revoked && !k.RevokedInFile {
			return errors.New("the key was revoked before")
		}
		if len(others(k.Fingerprint)) == 0 {
			return noAuth("the revocation of a key must be signed by a different authorised key: the key under revocation vouches for nothing")
		}
		since, err := utcTime(ev.Fields["compromised-since"])
		if err != nil {
			return fmt.Errorf("compromised-since: %w", err)
		}
		reason := ev.Fields["reason"]
		if reason == "" || len(reason) > MaxReason {
			return fmt.Errorf("reason must be 1 to %d printable characters", MaxReason)
		}
		k.Revoked, k.RevokedBy, k.CompromisedSince, k.RevokeReason = true, seq, &since, reason
		k.MaxTreeSize = nil
	case TypeChainRetired:
		return w.chainRetired(ev, authorised)
	case TypeEpochClosed:
		return w.epochClosed(ev, authorised)
	case TypeRestoreAcknowledged:
		return w.restoreAcknowledged(ev, authorised)
	default:
		w.Other[ev.Type]++
	}
	return nil
}

func (w *Walk) keyOf(ev *Event, byFP map[string]*Key) (*Key, *uint64, error) {
	b64, ok := ev.Fields["key"]
	if !ok {
		return nil, nil, errors.New("the event has no key line")
	}
	pub, err := signednote.DecodeBase64(b64)
	if err != nil || len(pub) != ed25519.PublicKeySize {
		return nil, nil, errors.New("the key line is not a canonical base64 public key of 32 bytes")
	}
	sum := sha256.Sum256(pub)
	k := byFP[hex.EncodeToString(sum[:])]
	if k == nil {
		return nil, nil, fmt.Errorf("the event is about key %s, which no earlier event or the trust file introduced", hex.EncodeToString(sum[:])[:16])
	}
	var bound *uint64
	if v, ok := ev.Fields["max-tree-size"]; ok && v != "unbounded" {
		n, err := canonicalUint(v, tlog.MaxTreeSize)
		if err == nil && n >= 1 {
			bound = &n
		}
	}
	if ev.Type == TypeKeyRetired {
		if _, ok := ev.Fields["max-tree-size"]; !ok {
			return nil, nil, errors.New("the event has no max-tree-size line")
		}
	}
	return k, bound, nil
}

func (w *Walk) parseAdded(ev *Event, byFP map[string]*Key) (*Key, error) {
	b64, ok := ev.Fields["key"]
	if !ok {
		return nil, errors.New("the event has no key line")
	}
	pub, err := signednote.DecodeBase64(b64)
	if err != nil || len(pub) != ed25519.PublicKeySize {
		return nil, errors.New("the key line is not a canonical base64 public key of 32 bytes")
	}
	if err := edkey.Check(pub); err != nil {
		return nil, fmt.Errorf("the key the event adds is refused: %w", err)
	}
	fp := trust.Fingerprint(pub)
	if ev.Fields["fingerprint"] != fp {
		return nil, fmt.Errorf("the fingerprint line is %q, expected %s, the SHA-256 of the key", ev.Fields["fingerprint"], fp)
	}
	nb, err := utcTime(ev.Fields["not-before"])
	if err != nil {
		return nil, fmt.Errorf("not-before: %w", err)
	}
	if k := byFP[fp]; k != nil {
		return k, nil
	}
	return &Key{Name: "added-" + strconv.FormatUint(ev.Seq, 10), Public: pub, Fingerprint: fp, AddedBy: ev.Seq, NotBefore: &nb}, nil
}

func (w *Walk) keyAdded(ev *Event, cand *Key, possession bool, authorised []*Key, byFP map[string]*Key) error {
	if existing := byFP[cand.Fingerprint]; existing != nil {
		if !existing.Pinned {
			return errors.New("the key was added before")
		}
		// the event of a key the auditor pinned (the first key's own event, or a later copy): it needs
		// the key's own signature or another authorised one, and adds nothing
		if !possession && len(authorised) == 0 {
			return noAuth("the event of a pinned key is signed by neither that key nor another authorised key")
		}
		return nil
	}
	if !possession {
		return noAuth("the event adds key %s and is not signed by that key: the new key must sign its own event to show that it holds the private key", cand.Fingerprint[:16])
	}
	var vouching []*Key
	for _, k := range authorised {
		if k.Fingerprint != cand.Fingerprint {
			vouching = append(vouching, k)
		}
	}
	if len(vouching) == 0 {
		return noAuth("the event adds key %s and no authorised key other than the new one signed it: trust moves from a key the auditor pinned, never from the new key alone", cand.Fingerprint[:16])
	}
	if len(w.Keys) >= trust.MaxKeys {
		return fmt.Errorf("the walk holds %d keys already, expected at most %d", len(w.Keys), trust.MaxKeys)
	}
	w.Keys = append(w.Keys, cand)
	byFP[cand.Fingerprint] = cand
	return nil
}

func (w *Walk) chainRetired(ev *Event, authorised []*Key) error {
	want := map[string]bool{"chain": true, "epoch": true, "final-height": true, "final-block-hash": true, "reason": true}
	for name := range ev.Fields {
		switch name {
		case "seq", "time", "type":
		default:
			if !want[name] {
				return fmt.Errorf("the line %q is not one of a chain-retired event", name)
			}
		}
	}
	var cr ChainRetired
	var err error
	cr.Seq = ev.Seq
	if cr.Chain, err = hex32(ev.Fields["chain"]); err != nil {
		return fmt.Errorf("chain: %w", err)
	}
	e, err := canonicalUint(ev.Fields["epoch"], 1<<31)
	if err != nil || e < 1 {
		return fmt.Errorf("epoch %q is not a number from 1", ev.Fields["epoch"])
	}
	cr.Epoch = uint32(e) // #nosec G115 -- range checked
	if cr.FinalHeight, err = canonicalUint(ev.Fields["final-height"], tlog.MaxTreeSize-1); err != nil {
		return fmt.Errorf("final-height: %w", err)
	}
	if cr.FinalBlockHash, err = hex32(ev.Fields["final-block-hash"]); err != nil {
		return fmt.Errorf("final-block-hash: %w", err)
	}
	switch r := ev.Fields["reason"]; r {
	case "dropped", "group-removed", "database-missing", "chain-replaced":
		cr.Reason = r
	default:
		return fmt.Errorf("reason %q is not one of dropped, group-removed, database-missing, chain-replaced", r)
	}
	key := ChainKey(cr.Chain, cr.Epoch)
	if _, dup := w.Retired[key]; dup {
		return fmt.Errorf("the chain %s is retired twice", key)
	}
	for _, k := range authorised {
		cr.SignedBy = append(cr.SignedBy, k.Fingerprint)
	}
	w.Retired[key] = cr
	return nil
}

var _ = safefile.ErrTooLarge

// skip records an event that was well formed and not authorised.
func (w *Walk) skip(seq uint64, raw []byte, ae *authError) {
	sk := Skipped{Seq: seq, Reason: ae.msg}
	if n, err := signednote.Parse(raw); err == nil {
		if ev, err := Parse(n.Text); err == nil {
			sk.Type = ev.Type
			if ev.Type == TypeEpochClosed {
				if c, err := hex32(ev.Fields["chain"]); err == nil {
					if e, err := canonicalUint(ev.Fields["epoch"], 1<<31); err == nil && e >= 1 {
						w.UnauthorisedEpochClosed[ChainKey(c, uint32(e))] = seq // #nosec G115 -- range checked
					}
				}
			}
			if ev.Type == TypeChainRetired {
				if c, err := hex32(ev.Fields["chain"]); err == nil {
					if e, err := canonicalUint(ev.Fields["epoch"], 1<<31); err == nil && e >= 1 {
						w.UnauthorisedRetired[ChainKey(c, uint32(e))] = seq // #nosec G115 -- range checked
					}
				}
			}
		}
	}
	w.Skipped = append(w.Skipped, sk)
}
