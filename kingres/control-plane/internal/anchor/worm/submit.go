// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package worm

import (
	"bytes"
	"context"
	"crypto/md5" //nolint:gosec // Content-MD5 is the integrity header S3 asks for on PutObjectRetention
	"encoding/base64"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/anchor"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/anchor/sigv4"
)

// Bounds of the time checks and of the search for our version.
const (
	// DefaultMaxSkew is the tolerance of the store's time: Last-Modified of a
	// fresh write against the clock of Submit, and Last-Modified of an
	// existing object against the start of the first Submit call (below) and the
	// clock.
	DefaultMaxSkew = 5 * time.Minute
	// RetentionTolerance is how much earlier than asked the retention date
	// of an object may be (the store rounds, and the write takes time).
	RetentionTolerance = 5 * time.Minute
	// MaxVersionsChecked bounds the versions of one key that are read.
	MaxVersionsChecked = 25
	// MaxListPages bounds the pages of a version listing.
	MaxListPages = 5
	// MaxListBody is the most read from a version listing.
	MaxListBody = 1 << 20
)

func (t *Target) maxSkew() time.Duration {
	if t.s.cfg.MaxSkewSeconds > 0 {
		return time.Duration(t.s.cfg.MaxSkewSeconds) * time.Second
	}
	return DefaultMaxSkew
}

// errRepairable marks a not_locked error that PutObjectRetention can mend.
var errRepairable = errors.New("worm: the lock can be applied to the existing version")

// Submit implements anchor.Target (design 6.3, steps 1 to 5, with the
// correction 63). It is idempotent: when the key exists with the same
// bytes it verifies and returns that object (a crash before the record was
// written). A key that holds only other bytes gives an *anchor.Error of
// ClassConflict and nothing is overwritten; a foreign later version over the
// key is not a conflict as long as our version is there, locked (the
// versions of the key are searched).
//
// An object that an earlier Submit left without a lock (a store that accepted
// the write and kept no lock) is completed by a later Submit: the retention is
// applied to that version with PutObjectRetention, and when the store refuses,
// the error says exactly what the operator must do.
//
// The evidence carries the version that was read back, the SHA-256 of its
// bytes and their length, the store's Last-Modified, the SubmitStart that was
// applied, the attempt that was used for the window (null for a version without
// the metadata), the generation of the key, and whether the version was found or
// written by this call. The key is ObjectKeyGen of the payload's generation. The
// read-back of a version this call wrote must return the attempt as sent
// ("no_user_metadata" otherwise), a malformed value on a found version is
// "attempt_malformed", and when the latest version with our bytes fails its
// checks the other versions are looked at (correction 95).
//
// The time of the store is bounded against our clock and the start of the
// first Submit call for the key (p.SubmitStart, which the caller made durable
// before that call and never changes; correction 89). An object that this call
// has just written must have a time within the tolerance (Config.MaxSkewSeconds)
// of now. An existing object (a later call, a crash recovery, a Retry) must
// have a time not earlier than SubmitStart less the tolerance and not later than
// now plus the tolerance; when it carries the start of the call that wrote it
// (the user metadata "kingres-attempt", which every put of this package sets),
// the window is also at most that start less the tolerance to that start plus
// CallTimeout plus the tolerance, so that a Retry or a crash recovery days
// after SubmitStart does not widen it (a version without that metadata, one that
// another principal wrote, has only the SubmitStart window). Whatever the age
// of the record, the number of attempts or the delay, the time the evidence
// states is therefore never further from the true time of the write than the
// tolerance plus the duration of the call that wrote it, for a version this
// package wrote, and never earlier than SubmitStart less the tolerance for any
// version. A store whose clock was refused for a fresh write is refused on every
// later attempt for that object (Code store_time_early, permanent;
// store_time_skew, refused, when the store is ahead).
func (t *Target) Submit(ctx context.Context, p anchor.Payload) (anchor.Receipt, error) {
	if err := anchor.CheckPayload(anchor.KindWORM, p); err != nil {
		return anchor.Receipt{}, err
	}
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	cfg := t.s.cfg
	key := ObjectKeyGen(cfg.KeyPrefix, p.AggregateID(), p.Size, p.Generation)
	now := t.s.clock()
	days := time.Duration(cfg.RetentionDays) * 24 * time.Hour
	start := p.SubmitStart.UTC().Truncate(time.Second)
	if start.After(now.Add(t.maxSkew())) {
		return anchor.Receipt{}, anchor.Errorf(anchor.ClassRetryable, anchor.KindWORM, "submit", "submit_start_ahead",
			"the start of the first Submit call for this key (%s) is later than the clock of this server (%s) plus the tolerance of %s: check the clock of this server and of the server that stated the start, and that the start is the one persisted before the first call",
			anchor.FormatTime(start), anchor.FormatTime(now), t.maxSkew())
	}
	w := want{
		retain:  now.Add(days).UTC().Truncate(time.Second),
		start:   start,
		attempt: now.UTC().Truncate(time.Second),
		gen:     p.Generation,
	}
	body := p.SignedNote

	useINM := cfg.IfNoneMatch != IfNoneMatchOff
	if !useINM {
		return t.checkThenPut(ctx, key, body, w)
	}
	r, err := t.put(ctx, key, body, w.retain, true, true, w.attempt)
	if err != nil {
		return anchor.Receipt{}, err
	}
	switch {
	case r.status == 200:
		return t.verify(ctx, key, body, r.header.Get(hdrVersion), r.header.Get("ETag"), w, true)
	case r.status == 412 || r.status == 409:
		code, _ := ParseS3Error(r.body)
		if r.status == 409 && code != "ConditionalRequestConflict" {
			return anchor.Receipt{}, t.s.statusError("put", r)
		}
		return t.existing(ctx, key, body, w, r.status == 409)
	case (r.status == 501 || r.status == 400) && cfg.IfNoneMatch == IfNoneMatchAuto:
		code, msg := ParseS3Error(r.body)
		if r.status == 400 && isLockComplaint(code, msg) {
			return anchor.Receipt{}, t.s.statusError("put", r)
		}
		return t.checkThenPut(ctx, key, body, w)
	case (r.status == 501 || r.status == 400) && cfg.IfNoneMatch == IfNoneMatchOn:
		code, msg := ParseS3Error(r.body)
		if r.status == 400 && isLockComplaint(code, msg) {
			return anchor.Receipt{}, t.s.statusError("put", r)
		}
		e := t.s.statusError("put", r)
		e.Class, e.Code = anchor.ClassRefused, "if_none_match_unsupported"
		return anchor.Receipt{}, e
	}
	return anchor.Receipt{}, t.s.statusError("put", r)
}

// checkThenPut is the fallback without If-None-Match: HEAD the key, then
// put when it is absent. It is not atomic; object lock is what keeps an
// existing version safe.
func (t *Target) checkThenPut(ctx context.Context, key string, body []byte, w want) (anchor.Receipt, error) {
	h, err := t.head(ctx, "check", key, "")
	if err != nil {
		return anchor.Receipt{}, err
	}
	switch h.status {
	case 200:
		return t.existing(ctx, key, body, w, false)
	case 404:
	default:
		return anchor.Receipt{}, t.s.statusError("check", h)
	}
	r, err := t.put(ctx, key, body, w.retain, false, true, w.attempt)
	if err != nil {
		return anchor.Receipt{}, err
	}
	if r.status != 200 {
		return anchor.Receipt{}, t.s.statusError("put", r)
	}
	return t.verify(ctx, key, body, r.header.Get(hdrVersion), r.header.Get("ETag"), w, true)
}

// existing handles "the key is already there": it looks for OUR version, the
// one whose bytes are the note. The latest version is read first; when it is
// not ours (a foreign later version, or a delete marker) the versions of the
// key are listed and each one with the bytes of the note is tried. Only when
// no version of the key holds the note is it a conflict.
func (t *Target) existing(ctx context.Context, key string, body []byte, w want, transient bool) (anchor.Receipt, error) {
	g, err := t.get(ctx, "get", key, "")
	if err != nil {
		var ae *anchor.Error
		if !(errors.As(err, &ae) && errors.Is(ae.Err, anchor.ErrTooLarge)) {
			return anchor.Receipt{}, err
		}
		g = nil // a larger object is not ours; look at the other versions
	}
	// firstErr is the verdict on the latest version when it has our bytes and
	// fails its checks: that version does not decide alone (correction 95), the
	// other versions of the key are looked at, and the first of ours that passes
	// completes the record. A foreign copy of our bytes placed after ours (the
	// note is public) cannot end the record.
	var firstErr error
	skip := ""
	if g != nil {
		switch {
		case g.status == 200 && bytes.Equal(g.body, body):
			skip = g.header.Get(hdrVersion)
			rc, err := t.complete(ctx, key, body, skip, g.header.Get("ETag"), w)
			if err == nil {
				return rc, nil
			}
			if ctx.Err() != nil {
				return anchor.Receipt{}, err
			}
			firstErr = err
		case g.status != 200 && g.status != 404:
			return anchor.Receipt{}, t.s.statusError("get", g)
		}
	}
	vers, bounded, err := t.ourVersions(ctx, key, body)
	if err != nil {
		if firstErr != nil {
			return anchor.Receipt{}, firstErr
		}
		return anchor.Receipt{}, err
	}
	if skip != "" {
		kept := vers[:0:0]
		for _, v := range vers {
			if v != skip {
				kept = append(kept, v)
			}
		}
		vers = kept
	}
	if len(vers) == 0 && firstErr != nil {
		return anchor.Receipt{}, boundReached(firstErr, bounded)
	}
	if len(vers) == 0 {
		switch {
		case g != nil && g.status == 404 && transient:
			return anchor.Receipt{}, anchor.Errorf(anchor.ClassRetryable, anchor.KindWORM, "get", "conflicting_request", "a conflicting request is in progress")
		case g != nil && g.status == 404:
			return anchor.Receipt{}, anchor.Errorf(anchor.ClassPermanent, anchor.KindWORM, "get", "inconsistent", "the store reports the key as existing and as absent")
		}
		return anchor.Receipt{}, conflict()
	}
	for _, v := range vers {
		rc, err := t.complete(ctx, key, body, v, "", w)
		if err == nil {
			return rc, nil
		}
		if firstErr == nil {
			firstErr = err
		}
		if ctx.Err() != nil {
			break
		}
	}
	return anchor.Receipt{}, boundReached(firstErr, bounded)
}

// complete verifies an existing version and, when it has no lock (or a
// shorter retention than asked, in the configured mode), applies the retention
// to that version and verifies again.
func (t *Target) complete(ctx context.Context, key string, body []byte, version, etag string, w want) (anchor.Receipt, error) {
	rc, err := t.verify(ctx, key, body, version, etag, w, false)
	var rn *repairNeeded
	if err == nil || !errors.As(err, &rn) {
		return rc, err
	}
	if rerr := t.applyRetention(ctx, key, version, rn.mode, w.retain); rerr != nil {
		return anchor.Receipt{}, rerr
	}
	return t.verify(ctx, key, body, version, etag, w, false)
}

type retentionXML struct {
	XMLName         xml.Name `xml:"Retention"`
	Xmlns           string   `xml:"xmlns,attr"`
	Mode            string   `xml:"Mode"`
	RetainUntilDate string   `xml:"RetainUntilDate"`
}

// putRetention sends PutObjectRetention for one version and returns the
// response (any status), or the error of the exchange.
func (t *Target) putRetention(ctx context.Context, key, version, mode string, retain time.Time) (*response, error) {
	body, err := xml.Marshal(retentionXML{Xmlns: "http://s3.amazonaws.com/doc/2006-03-01/", Mode: mode, RetainUntilDate: anchor.FormatTime(retain)})
	if err != nil {
		return nil, anchor.Errorf(anchor.ClassPermanent, anchor.KindWORM, "retention", "request", "request could not be built")
	}
	req, err := t.s.request(ctx, http.MethodPut, key, "retention&versionId="+sigv4.Escape(version, false), body)
	if err != nil {
		return nil, anchor.Errorf(anchor.ClassPermanent, anchor.KindWORM, "retention", "request", "request could not be built")
	}
	sum := md5.Sum(body) //nolint:gosec
	req.Header.Set("Content-MD5", base64.StdEncoding.EncodeToString(sum[:]))
	req.Header.Set("Content-Type", "application/xml")
	return t.s.do(ctx, "retention", req, body, MaxErrorBody)
}

// applyRetention is PutObjectRetention on one version. A store that refuses
// gives a permanent error that says what the operator must do; a transport
// failure is retryable.
func (t *Target) applyRetention(ctx context.Context, key, version, mode string, retain time.Time) error {
	r, err := t.putRetention(ctx, key, version, mode, retain)
	if err != nil {
		return err
	}
	if r.status == 200 || r.status == 204 {
		return nil
	}
	se := t.s.statusError("retention", r)
	if se.Class == anchor.ClassRetryable {
		return se
	}
	return anchor.Errorf(anchor.ClassPermanent, anchor.KindWORM, "retention", "not_locked",
		"the object %s (version %s) has no lock that meets the configuration (it has no lock, or a lock that is too short or has expired) and the store refused to apply retention (http %d): apply the %s retention until %s to that version with the store's tools, then Retry; or use another keyPrefix",
		(anchor.Redactor{}).Clean(key, 300), (anchor.Redactor{}).Clean(version, 200), r.status, mode, anchor.FormatTime(retain))
}

type listResult struct {
	XMLName     xml.Name `xml:"ListVersionsResult"`
	IsTruncated bool     `xml:"IsTruncated"`
	NextKey     string   `xml:"NextKeyMarker"`
	NextVersion string   `xml:"NextVersionIdMarker"`
	Versions    []struct {
		Key       string `xml:"Key"`
		VersionID string `xml:"VersionId"`
	} `xml:"Version"`
}

// ourVersions lists the versions of key (newest first) and returns the ids of
// those whose bytes are the note, at most MaxVersionsChecked read.
func (t *Target) ourVersions(ctx context.Context, key string, body []byte) (ids []string, bounded bool, err error) {
	read := 0
	keyMarker, verMarker := "", ""
	for page := 0; page < MaxListPages; page++ {
		q := "prefix=" + sigv4.Escape(key, false) + "&versions"
		if keyMarker != "" {
			q += "&key-marker=" + sigv4.Escape(keyMarker, false)
		}
		if verMarker != "" {
			q += "&version-id-marker=" + sigv4.Escape(verMarker, false)
		}
		req, err := t.s.request(ctx, http.MethodGet, "", q, nil)
		if err != nil {
			return nil, false, anchor.Errorf(anchor.ClassPermanent, anchor.KindWORM, "list", "request", "request could not be built")
		}
		r, err := t.s.do(ctx, "list", req, nil, MaxListBody)
		if err != nil {
			return nil, false, err
		}
		if r.status != 200 {
			return nil, false, t.s.statusError("list", r)
		}
		var lr listResult
		dec := xml.NewDecoder(bytes.NewReader(r.body))
		dec.Strict = false
		dec.Entity = nil
		if err := dec.Decode(&lr); err != nil {
			return nil, false, anchor.Errorf(anchor.ClassPermanent, anchor.KindWORM, "list", "bad_listing", "the version listing is not readable")
		}
		for _, v := range lr.Versions {
			if v.Key != key || v.VersionID == "" || v.VersionID == "null" {
				continue
			}
			if read >= MaxVersionsChecked {
				return ids, true, nil
			}
			read++
			g, err := t.get(ctx, "get", key, v.VersionID)
			if err != nil {
				var ae *anchor.Error
				if errors.As(err, &ae) && errors.Is(ae.Err, anchor.ErrTooLarge) {
					continue
				}
				return nil, false, err
			}
			if g.status == 200 && bytes.Equal(g.body, body) {
				ids = append(ids, v.VersionID)
			}
		}
		if !lr.IsTruncated {
			break
		}
		keyMarker, verMarker = lr.NextKey, lr.NextVersion
		if page == MaxListPages-1 {
			bounded = true
		}
	}
	return ids, bounded, nil
}

// boundReached is the error returned when no version with our bytes passed its
// checks and the walk over the versions of the key stopped at its bound
// (MaxVersionsChecked versions read, or MaxListPages pages): our own version may
// be hidden behind later copies of our bytes, so the error of the latest copy,
// which says that another principal wrote it, is not a verdict on our object and
// must not send the operator to a new key generation (correction 105). It keeps
// the class of that error (a retryable one is returned as it is: it is a
// transport fault, not a verdict) and names the bound.
func boundReached(err error, bounded bool) error {
	var ae *anchor.Error
	if !bounded || !errors.As(err, &ae) || ae.Class == anchor.ClassRetryable {
		return err
	}
	return &anchor.Error{Class: ae.Class, Target: anchor.KindWORM, Op: "verify", Code: "versions_bound_reached", Err: err,
		Text: fmt.Sprintf("the search of the versions of this key reached its bound (at most %d versions read, %d list pages) and none of the versions with this note that were read passed its checks: our own version may be hidden behind later copies of the same bytes that another principal wrote, so this is not a verdict on our object; a new key generation does not help (it writes one more object that can be hidden in the same way): find who can write this bucket and stop it (the bucket must be written by nobody else), then Retry, or change the keyPrefix of the target; the latest copy failed with the code %s",
			MaxVersionsChecked, MaxListPages, sanitize(ae.Code))}
}

func conflict() *anchor.Error {
	return anchor.Errorf(anchor.ClassConflict, anchor.KindWORM, "put", "worm_conflict",
		"the bucket already holds a different note under this key and none of its versions is this note: a different checkpoint of the same size exists")
}

func notLocked(format string, args ...any) *anchor.Error {
	return anchor.Errorf(anchor.ClassPermanent, anchor.KindWORM, "verify", "not_locked", "the store did not lock the object: "+format, args...)
}

// repairNeeded is the cause of a repairable not_locked error: it matches
// errRepairable, and carries the lock mode the retention is applied in (the mode
// of the found version, or the configured one when it has no lock).
type repairNeeded struct{ mode string }

func (*repairNeeded) Error() string { return errRepairable.Error() }

func (*repairNeeded) Is(target error) bool { return target == errRepairable }

func repairable(e *anchor.Error, mode string) *anchor.Error {
	e.Err = &repairNeeded{mode: mode}
	return e
}

func mismatch(what string) *anchor.Error {
	return anchor.Errorf(anchor.ClassPermanent, anchor.KindWORM, "verify", "readback_mismatch", "read back differs from what was written: %s", what)
}

// want is what one Submit asks of the store and of the object it finds.
type want struct {
	// retain is the retention date asked now (also what a repair applies).
	retain time.Time
	// start is Payload.SubmitStart: the start of the first Submit call for the
	// key, as the caller stated it.
	start time.Time
	// attempt is the clock at the start of this call: it is written into the
	// object as user metadata (hdrAttempt), and a later call measures the
	// object by it (see verify).
	attempt time.Time
	// gen is Payload.Generation (the key's generation, recorded in the evidence).
	gen uint32
}

// attemptOf reads hdrAttempt of a version (correction 95): present is false when
// the store returned no such header (a version that another principal wrote, or
// a store that keeps no user metadata); when it is present it must be exactly
// one value in the strict TimeLayout that this package writes, else err is set:
// a value that is present and malformed (another format, several values, a
// joined list) is an error for that version and is never judged as absent.
func attemptOf(h http.Header) (a time.Time, present bool, err error) {
	vals, ok := h[hdrAttempt]
	if !ok {
		return time.Time{}, false, nil
	}
	if len(vals) != 1 {
		return time.Time{}, true, fmt.Errorf("%d values", len(vals))
	}
	a, err = anchor.ParseTime(vals[0])
	if err != nil {
		return time.Time{}, true, errors.New("not the exact format this package writes")
	}
	return a, true, nil
}

// timeAhead is the error for a store time later than the time it can have
// been written. The object stays as it is. It is refused (tried again after 1, 5
// and 15 minutes), not permanent, and the code and the text carry the cause:
// this is not a problem of credentials. ref says what the time is measured
// against. final is true for an object that carries the start of the call that
// wrote it (and for one this call wrote): its verdict is the same on every later
// call, so the text says that waiting and a Retry will not heal it, and what the
// operator can do (correction 95); for an object without it the clock catching up
// can make the time acceptable.
func timeAhead(what string, d time.Duration, max time.Duration, ref string, final bool) *anchor.Error {
	if final {
		e := anchor.Errorf(anchor.ClassRefused, anchor.KindWORM, "verify", "store_time_skew",
			"%s is %s ahead of %s, more than the tolerance of %s: the time the store states is not accepted; this is a problem of clocks, not of credentials; the object is locked, keeps its time and carries the start of the call that wrote it, so waiting and a Retry find the same object and are refused in the same way: correct the clock of the store or of this server, then either anchor this checkpoint under a new key generation (the caller raises the generation of the payload by one, with a new start; an operator's explicit choice, since every generation is one more object in the bucket that claims to be this anchor) or change the keyPrefix of the target",
			what, d.Round(time.Second), ref, max)
		// the verdict is the same on every later call: the record fails at once
		// (anchor.Record.AfterError), with the alert of a failed record
		e.Final = true
		return e
	}
	return anchor.Errorf(anchor.ClassRefused, anchor.KindWORM, "verify", "store_time_skew",
		"%s is %s ahead of %s, more than the tolerance of %s: the time the store states is not accepted; this is a problem of clocks, not of credentials: correct the clock of the store or of this server, then Retry (the object carries no start of its call, so a clock that has caught up can make its time acceptable)",
		what, d.Round(time.Second), ref, max)
}

// timeEarly is the error for a store time earlier than the start it is
// measured against (the start of the first Submit call for the key, or the
// start of the call that wrote the object) less the tolerance: the store's
// clock is behind, or the object is older than the first call for this key. The
// object is locked and keeps its time, and every later attempt finds the same
// object, so nothing is gained by trying again: permanent, with what the
// operator can do.
func timeEarly(what string, lmt, ref time.Time, refName string, max time.Duration) *anchor.Error {
	return anchor.Errorf(anchor.ClassPermanent, anchor.KindWORM, "verify", "store_time_early",
		"%s (%s) is earlier than %s (%s) by more than the tolerance of %s: the clock of the store is behind, or the object was written before the first Submit call for this key; this is a problem of clocks, not of credentials; the object is locked and keeps its time, and a Retry finds the same object, so correct the clock of the store, then either anchor this checkpoint under a new key generation (the caller raises the generation of the payload by one, with a new start; an operator's explicit choice, since every generation is one more object in the bucket that claims to be this anchor) or change the keyPrefix of the target (a new key has a new start), or, if the object is a valid earlier anchor of this checkpoint, create the record with the start of the first Submit call that wrote it",
		what, anchor.FormatTime(lmt), refName, anchor.FormatTime(ref), max)
}

// noMetadata is the error for a fresh write whose read-back does not carry the
// attempt exactly as it was sent (n values came back: 0 none, otherwise a
// changed or repeated value). The object is written, locked and read back, so it
// stays; the class is permanent so that the record does not spin. What a Retry
// does depends on n (correction 105): with n = 0 it finds the object, which has
// no such header, and accepts it under the wide window (the evidence then states
// attempt null); with n above 0 the object carries the changed or repeated value,
// so the Retry meets attempt_malformed, which is refused for good, and a new key
// generation writes another object that meets this refusal again: only another
// store, or a gateway that keeps the header exactly, helps. The two texts say so.
func noMetadata(n int) *anchor.Error {
	if n == 0 {
		return anchor.Errorf(anchor.ClassPermanent, anchor.KindWORM, "verify", "no_user_metadata",
			"this store does not keep user metadata: the object was written and locked with the user metadata x-amz-meta-kingres-attempt, and the read-back returned no such header (it was dropped), so the time of the store cannot be bounded by the call that wrote it; the object stays in the bucket until its retention ends; use a store (and a gateway in front of it) that keeps user metadata; a Retry finds this object and accepts it under the wider window of the start of the first Submit call only, and the evidence then states attempt null")
	}
	return anchor.Errorf(anchor.ClassPermanent, anchor.KindWORM, "verify", "no_user_metadata",
		"this store, or a gateway in front of it, changes or repeats user metadata: the object was written and locked with the user metadata x-amz-meta-kingres-attempt, and the read-back returned a value that is not the one sent (changed or repeated), so the time of the store cannot be bounded by the call that wrote it; the object stays in the bucket until its retention ends and carries the changed value; a Retry is refused for good (attempt_malformed), and a new key generation does not help: it writes another object that meets the same refusal and leaves one more locked object; only another store, or a gateway that returns the header exactly as it was sent, helps")
}

// verify reads one version back (design 6.3 step 3): HEAD of the version for
// the lock mode, the retention date, Last-Modified, length, checksum and
// version id; GET of that version for the bytes. fresh is true for an object
// this call has just written: the retention date may then be at most
// RetentionTolerance earlier than asked now and Last-Modified must be within
// the tolerance of the clock (not earlier than now less it, not later than now
// plus it). An existing object (fresh false; correction 89) must have a
// Last-Modified not earlier than SubmitStart less the tolerance and not later
// than now plus the tolerance, narrowed, when the object carries the start of
// the call that wrote it, to that start less the tolerance up to that start
// plus CallTimeout plus the tolerance; it has no other age limit. Its retention
// date must be not earlier than its own Last-Modified plus the configured days,
// less the tolerance of the time and RetentionTolerance (and not in the past),
// else it is repairable: the floor comes from the store's own timeline, not
// from a time the caller states.
func (t *Target) verify(ctx context.Context, key string, body []byte, version, etag string, w want, fresh bool) (anchor.Receipt, error) {
	retry := func(e *anchor.Error) (anchor.Receipt, error) {
		// a failed read-back after a good write is not a verdict on the
		// object: a transport failure or a 5xx is retried with backoff; any
		// other failure of the read (a 404 for a version that was just
		// written, a 4xx, an answer that cannot be read) counts as refused,
		// so that it ends after a few tries instead of for ever
		if e.Class == anchor.ClassPermanent && e.Code != "redirect" {
			e = &anchor.Error{Class: anchor.ClassRefused, Target: e.Target, Op: "verify", Code: "verify_read_failed",
				Text: "the object was written but could not be read back: " + e.Text, Status: e.Status, Err: e}
		}
		return anchor.Receipt{}, e
	}
	h, err := t.head(ctx, "verify-head", key, version)
	if err != nil {
		return retry(err.(*anchor.Error))
	}
	if h.status != 200 {
		return retry(t.s.statusError("verify-head", h))
	}
	if version == "" {
		version = h.header.Get(hdrVersion)
	} else if v := h.header.Get(hdrVersion); v != "" && v != version {
		return anchor.Receipt{}, mismatch("version id")
	}
	if version == "" {
		return anchor.Receipt{}, anchor.Errorf(anchor.ClassPermanent, anchor.KindWORM, "verify", "no_version_id",
			"the store returned no version id: object lock needs a bucket with versioning, and the evidence names the version")
	}
	// the time of the store first: nothing is repaired on an object whose
	// time is refused
	lm := h.header.Get("Last-Modified")
	lmt, err2 := http.ParseTime(lm)
	if lm == "" || err2 != nil {
		return anchor.Receipt{}, anchor.Errorf(anchor.ClassPermanent, anchor.KindWORM, "verify", "no_last_modified", "the store returned no usable Last-Modified")
	}
	lmt = lmt.UTC().Truncate(time.Second)
	now := t.s.clock()
	skew := t.maxSkew()
	what := "the Last-Modified of the existing object"
	// the window of the store's time is [lower, upperRef + skew]
	lower, lowerRef, lowerName := w.start.Add(-skew), w.start, "the start of the first Submit call for this key"
	upperRef, upperName := now, "the clock of this server"
	// attemptUsed is what the evidence states as the attempt that was applied:
	// nil for a version without the metadata; final is true when the version
	// carries (or this call just sent) the start of its call, so that every later
	// call judges it by that start
	var attemptUsed *string
	final := false
	if fresh {
		what = "the Last-Modified of the object"
		if l := now.Add(-skew); l.After(lower) {
			lower, lowerRef, lowerName = l, now, "the clock of this server"
		}
		// the evidence names the attempt that was applied: the one sent (the
		// echo is checked below)
		sent := anchor.FormatTime(w.attempt)
		attemptUsed = &sent
		final = true
	} else {
		a, present, aerr := attemptOf(h.header)
		if aerr != nil {
			return anchor.Receipt{}, anchor.Errorf(anchor.ClassPermanent, anchor.KindWORM, "verify", "attempt_malformed",
				"the version %s carries the user metadata %s, but it is not the value this package writes (%s): it is not judged as a version without the metadata; the object is locked and keeps the value, so a Retry finds the same version; there are two causes. If another principal wrote it, anchor this checkpoint under a new key generation (the caller raises the generation of the payload by one, with a new start; an operator's explicit choice) or change the keyPrefix of the target. If this package wrote it and the store or a gateway changed or repeated the header (an earlier attempt of this key ended in no_user_metadata: the last error of the record says so), a new key generation does not help, because it writes another object that meets the same refusal and leaves one more locked object: only another store, or a gateway that returns the header exactly as it was sent, helps",
				(anchor.Redactor{}).Clean(version, 200), "x-amz-meta-kingres-attempt", aerr.Error())
		}
		if present {
			// the version carries the start of the call that wrote it: a Retry or
			// a crash recovery long after SubmitStart judges it by that start
			s := anchor.FormatTime(a)
			attemptUsed = &s
			final = true
			if l := a.Add(-skew); l.After(lower) {
				lower, lowerRef, lowerName = l, a, "the start of the Submit call that wrote the object"
			}
			if u := a.Add(CallTimeout); u.Before(now) {
				upperRef, upperName = u, "the end of the Submit call that wrote the object"
			}
		}
	}
	if d := lmt.Sub(upperRef); d > skew {
		return anchor.Receipt{}, timeAhead(what, d, skew, upperName, final)
	}
	if lmt.Before(lower) {
		return anchor.Receipt{}, timeEarly(what, lmt, lowerRef, lowerName, skew)
	}
	if fresh {
		// the window of a version this call wrote was the tolerance around the
		// clock of the call, whatever the store does with the metadata; now the
		// store must also have kept the attempt exactly as sent, or the next call
		// would judge this version by the wide window without saying so
		// (correction 95)
		sent := anchor.FormatTime(w.attempt)
		if vals := h.header.Values(hdrAttempt); len(vals) != 1 || vals[0] != sent {
			return anchor.Receipt{}, noMetadata(len(vals))
		}
	}
	mode := h.header.Get(hdrMode)
	if mode == "" {
		return anchor.Receipt{}, repairable(notLocked("no lock mode on the object"), t.s.cfg.lockMode())
	}
	// the configured mode is accepted, and COMPLIANCE when GOVERNANCE is
	// configured (the stronger lock); GOVERNANCE where COMPLIANCE is configured
	// is refused, because a principal with the bypass permission could delete
	// the version (correction 89)
	if mode != t.s.cfg.lockMode() && !(mode == "COMPLIANCE" && t.s.cfg.lockMode() == "GOVERNANCE") {
		return anchor.Receipt{}, notLocked("lock mode is %s, configured %s: a weaker lock than the configured one is never accepted; change the mode of the configuration, or lock the version with the configured mode with the store's tools", sanitize(mode), t.s.cfg.lockMode())
	}
	ru := h.header.Get(hdrRetain)
	if ru == "" {
		return anchor.Receipt{}, repairable(notLocked("no retain-until date on the object"), mode)
	}
	rt, err2 := time.Parse(time.RFC3339Nano, ru)
	if err2 != nil {
		return anchor.Receipt{}, notLocked("retain-until date is not RFC 3339")
	}
	rt = rt.UTC().Truncate(time.Second)
	// what was asked: for a version this call wrote, the retention of this
	// call; for an existing one, what the store's own time and the configured
	// days give (a put at the store's Last-Modified, which our clock may have
	// been up to the tolerance away from), never a time that the caller states
	asked := w.retain
	if !fresh {
		asked = lmt.Add(time.Duration(t.s.cfg.RetentionDays)*24*time.Hour - skew)
		if !asked.Add(-RetentionTolerance).After(now) {
			// the version is older than its own retention period: a floor that
			// would be in the past protects nothing (a lock ending in a minute
			// would pass), so the version is held to what a fresh anchor gets,
			// now plus the configured days, and is repaired to it (correction 95)
			asked = w.retain
		}
	}
	min := asked.Add(-RetentionTolerance)
	if rt.Before(min) {
		e := notLocked("retain-until date %s is earlier than the configured retention requires (%s)", anchor.FormatTime(rt), anchor.FormatTime(min))
		if !fresh {
			repairable(e, mode)
		}
		return anchor.Receipt{}, e
	}
	if cl := h.header.Get("Content-Length"); cl != strconv.Itoa(len(body)) {
		return anchor.Receipt{}, mismatch("content length")
	}
	sum := sum64(body)
	if s := h.header.Get(hdrSum); s != "" && s != sum {
		return anchor.Receipt{}, mismatch("checksum")
	}
	if he := h.header.Get("ETag"); he != "" {
		etag = he
	}
	g, err := t.get(ctx, "verify-get", key, version)
	if err != nil {
		return retry(err.(*anchor.Error))
	}
	if g.status != 200 {
		return retry(t.s.statusError("verify-get", g))
	}
	if !bytes.Equal(g.body, body) {
		return anchor.Receipt{}, mismatch("bytes")
	}
	cfg := t.s.cfg
	raw := sha256Sum(body)
	ev := anchor.WORMEvidence{
		Endpoint: cfg.Endpoint, Bucket: cfg.Bucket, Key: key, VersionID: t.s.red.Clean(version, 1024),
		ETag: t.s.red.Clean(etag, 256), ObjectSHA256: hex.EncodeToString(raw[:]), ContentLength: uint64(len(body)),
		LockMode: mode, RetainUntil: anchor.FormatTime(rt),
		LastModified: anchor.FormatTime(lmt), SubmitStart: anchor.FormatTime(w.start), Attempt: attemptUsed,
		Generation: w.gen, Found: !fresh, RecordedBy: anchor.RecordedByCluster,
	}
	enc, err2 := anchor.EncodeEvidence(ev)
	if err2 != nil {
		return anchor.Receipt{}, anchor.Errorf(anchor.ClassPermanent, anchor.KindWORM, "verify", "bad_evidence", "the store's answer cannot be recorded as evidence")
	}
	return anchor.Receipt{State: anchor.ReceiptAnchored, Evidence: enc, EvidenceTime: &lmt}, nil
}

// VerifyNote is the check an independent verifier makes of stored evidence
// with the bytes of the note in hand (from the publication or fetched from
// the version): the note is exactly what the evidence says was written (SHA-256
// and length), it is a well-formed signed note whose text hashes to the digest
// payload_sha256 of the evidence file, and the key of the evidence is the key
// of that note (".../<aggregate id>/<size, 20 digits>.note"). aggregateID is
// the lower case hex id of the aggregate. It does not verify the signature
// (that is the verifier's own check under its pinned key) and it does not
// fetch anything: the evidence time and the lock stay claims of the cluster
// until the verifier's own fetch of the version returns them (VerifyHead).
func VerifyNote(ev anchor.WORMEvidence, aggregateID string, signedNote []byte, payloadSHA256 string) error {
	if err := ev.Validate(); err != nil {
		return err
	}
	sum := sha256Sum(signedNote)
	if hex.EncodeToString(sum[:]) != ev.ObjectSHA256 || uint64(len(signedNote)) != ev.ContentLength {
		return errors.New("worm: the note is not the object the evidence describes")
	}
	p, err := anchor.ParsePayload(aggregateID, signedNote)
	if err != nil {
		return fmt.Errorf("worm: %w", err)
	}
	if p.DigestHex() != payloadSHA256 {
		return errors.New("worm: the text of the note does not hash to the payload digest")
	}
	gen := ""
	if ev.Generation > 0 {
		gen = ".g" + strconv.FormatUint(uint64(ev.Generation), 10)
	}
	if !strings.HasSuffix(ev.Key, "/"+aggregateID+"/"+strings.Repeat("0", 20-len(strconv.FormatUint(p.Size, 10)))+strconv.FormatUint(p.Size, 10)+gen+".note") {
		return errors.New("worm: the key of the evidence is not the key of this note")
	}
	return nil
}

// VerifyHead compares the headers of a HEAD of the version that a verifier
// fetched itself with the evidence: version id, lock mode and retention date.
// A match turns "recorded by the cluster" into "seen by the verifier".
func VerifyHead(ev anchor.WORMEvidence, h http.Header) error {
	if err := ev.Validate(); err != nil {
		return err
	}
	if v := h.Get(hdrVersion); v != ev.VersionID {
		return errors.New("worm: the version id differs from the evidence")
	}
	if h.Get(hdrMode) != ev.LockMode {
		return errors.New("worm: the lock mode differs from the evidence")
	}
	rt, err := time.Parse(time.RFC3339Nano, h.Get(hdrRetain))
	if err != nil || anchor.FormatTime(rt) != ev.RetainUntil {
		return errors.New("worm: the retention date differs from the evidence")
	}
	if cl := h.Get("Content-Length"); cl != strconv.FormatUint(ev.ContentLength, 10) {
		return errors.New("worm: the length differs from the evidence")
	}
	return nil
}
