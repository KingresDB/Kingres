// SPDX-License-Identifier: AGPL-3.0-only

// Package wormv checks the evidence of a WORM anchor (ledger-phase7.md 6.3): an object that holds a signed
// checkpoint, written to a store under Object Lock. The evidence file is the cluster's claim ("recorded_by":
// "cluster"); what makes it more is fetching the object version yourself, from a URL you were given (anonymous or
// presigned), and comparing the bytes and the headers of the answer with the claim.
//
// Without a fetch nothing is checked against the store and the caller must say so: the key is only compared with
// the checkpoint it should name. With a fetch: the body must have the SHA-256 and the length of the evidence, be a note
// whose signature the caller verifies and whose text hashes to the payload digest; the headers of the answer
// (x-amz-version-id, x-amz-object-lock-mode, x-amz-object-lock-retain-until-date, Last-Modified) are compared with
// the evidence when the store returns them, and a header it does not return is said, not assumed.
package wormv

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/KingresDB/Kingres/kingres/verifier/internal/anchorev"
)

// MaxObject is the largest object read (a signed checkpoint is far below it).
const MaxObject = 1 << 20

// Object is what a fetch returned.
type Object struct {
	Body         []byte
	VersionID    string
	LockMode     string
	RetainUntil  *time.Time
	LastModified *time.Time
}

// ErrUnreachable is returned for a transport failure, a timeout or a 5xx or 429 answer: nothing is known.
var ErrUnreachable = errors.New("the store could not be reached")

// ErrStatus is returned for an answer that is not 200 (the store did not give the object).
type ErrStatus struct{ Code int }

func (e *ErrStatus) Error() string {
	return fmt.Sprintf("the store answered %d, not 200 (a 404 can mean that the version is gone, a 403 an expired or wrong URL)", e.Code)
}

func loopback(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// CheckURL refuses a URL this program will not call: it must be https, or http for this machine, with no user information
// and no fragment. A query is allowed (a presigned URL is one).
func CheckURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, errors.New("the URL is not valid")
	}
	switch {
	case u.Host == "" || u.Hostname() == "":
		return nil, errors.New("the URL has no host")
	case u.User != nil:
		return nil, errors.New("the URL carries a user name or password, which is refused")
	case u.Fragment != "" || strings.Contains(raw, "#"):
		return nil, errors.New("the URL has a fragment")
	case u.Scheme == "http" && !loopback(u.Hostname()):
		return nil, errors.New("plain http is accepted only for an address of this machine; use https")
	case u.Scheme != "http" && u.Scheme != "https":
		return nil, errors.New("the URL must be https")
	}
	return u, nil
}

// Fetch GETs the URL: no redirect is followed, no proxy is used, every phase has a timeout and the body is limited.
func Fetch(raw string) (*Object, error) {
	u, err := CheckURL(raw)
	if err != nil {
		return nil, err
	}
	tr := &http.Transport{Proxy: nil, DialContext: (&net.Dialer{Timeout: 10 * time.Second}).DialContext, TLSHandshakeTimeout: 10 * time.Second, ResponseHeaderTimeout: 15 * time.Second, DisableCompression: true}
	cl := &http.Client{Transport: tr, Timeout: 60 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	defer tr.CloseIdleConnections()
	req, err := http.NewRequest(http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, errors.New("the request could not be made")
	}
	resp, err := cl.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", ErrUnreachable, sanitize(err))
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode >= 300 && resp.StatusCode < 400:
		return nil, errors.New("the store answered with a redirect, which is never followed: give the address that serves the object")
	case resp.StatusCode >= 500 || resp.StatusCode == 429:
		return nil, fmt.Errorf("%w: status %d", ErrUnreachable, resp.StatusCode)
	case resp.StatusCode != http.StatusOK:
		return nil, &ErrStatus{resp.StatusCode}
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, MaxObject+1))
	if err != nil {
		return nil, fmt.Errorf("%w: %s", ErrUnreachable, sanitize(err))
	}
	if len(body) > MaxObject {
		return nil, fmt.Errorf("the object is larger than %d bytes", MaxObject)
	}
	o := &Object{Body: body, VersionID: resp.Header.Get("x-amz-version-id"), LockMode: resp.Header.Get("x-amz-object-lock-mode")}
	if v := resp.Header.Get("x-amz-object-lock-retain-until-date"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			return nil, errors.New("the retain-until header of the store is not an RFC 3339 time")
		}
		t = t.UTC()
		o.RetainUntil = &t
	}
	if v := resp.Header.Get("Last-Modified"); v != "" {
		t, err := http.ParseTime(v)
		if err != nil {
			return nil, errors.New("the Last-Modified header of the store is not an HTTP date")
		}
		t = t.UTC()
		o.LastModified = &t
	}
	return o, nil
}

// sanitize removes the URL (which may be presigned) from a transport error.
func sanitize(err error) string {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err.Error()
	}
	return err.Error()
}

// CheckKey compares the object key of the evidence with the checkpoint it should hold: it ends with
// "<aggregate id>/<size as 20 digits>.note" (generation 0) or "<aggregate id>/<size>.g<n>.note" (generation n), and the
// evidence says the same generation.
func CheckKey(f *anchorev.File, aggregateID string) error {
	w := f.WORM
	want := fmt.Sprintf("%s/%020d.note", aggregateID, f.Size)
	if w.Generation > 0 {
		want = fmt.Sprintf("%s/%020d.g%d.note", aggregateID, f.Size, w.Generation)
	}
	if w.Key != want && !strings.HasSuffix(w.Key, "/"+want) {
		return fmt.Errorf("the object key %q does not end in %q: the evidence names an object of another checkpoint (or generation)", w.Key, want)
	}
	return nil
}

// Compare compares a fetched object with the evidence. problems are contradictions (the evidence is not what the store
// holds); notes say what the store did not return, which is then only the cluster's word.
func Compare(w *anchorev.WORM, o *Object) (problems, notes []string) {
	sum := sha256.Sum256(o.Body)
	if uint64(len(o.Body)) != w.ContentLength {
		problems = append(problems, fmt.Sprintf("the object has %d bytes and the evidence says %d", len(o.Body), w.ContentLength))
	}
	if sum != w.ObjectSHA256 {
		problems = append(problems, fmt.Sprintf("the SHA-256 of the object is %x and the evidence says %x: this is not the object that was recorded", sum[:8], w.ObjectSHA256[:8]))
	}
	switch {
	case o.VersionID == "":
		notes = append(notes, "the store did not return x-amz-version-id: that this is the version the evidence names was not confirmed (the URL should name the version)")
	case o.VersionID != w.VersionID:
		problems = append(problems, fmt.Sprintf("the store answered with version %q and the evidence names %q: a different version of the key", o.VersionID, w.VersionID))
	}
	switch {
	case o.LockMode == "":
		notes = append(notes, "the store did not return x-amz-object-lock-mode: the lock is the cluster's word and was not confirmed (a store that does not lock is no anchor)")
	case o.LockMode != w.LockMode:
		problems = append(problems, fmt.Sprintf("the store says the lock mode is %s and the evidence says %s", o.LockMode, w.LockMode))
	}
	switch {
	case o.RetainUntil == nil:
		notes = append(notes, "the store did not return the retain-until date: it is the cluster's word")
	case !o.RetainUntil.Equal(w.RetainUntil):
		problems = append(problems, fmt.Sprintf("the store says the object is retained until %s and the evidence says %s", o.RetainUntil.Format(time.RFC3339), w.RetainUntil.Format(time.RFC3339)))
	}
	switch {
	case o.LastModified == nil:
		notes = append(notes, "the store did not return Last-Modified: the time of the object is the cluster's word")
	case !o.LastModified.Equal(w.LastModified):
		problems = append(problems, fmt.Sprintf("the store says the object was last modified at %s and the evidence says %s", o.LastModified.Format(time.RFC3339), w.LastModified.Format(time.RFC3339)))
	}
	return problems, notes
}

// Confirmed reports whether the answer confirmed the lock (mode and retain-until).
func Confirmed(o *Object) bool { return o.LockMode != "" && o.RetainUntil != nil }
