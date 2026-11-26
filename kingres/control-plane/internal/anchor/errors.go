// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package anchor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// Class is how the caller must treat an error (section 6.6 of the design).
type Class int

const (
	// ClassRetryable: unreachable, timeout, 5xx, 429, a calendar or TSA that
	// says "waiting". The record stays pending and is tried again with backoff.
	ClassRetryable Class = iota + 1
	// ClassPermanent: retrying cannot help (a malformed answer, a store that
	// accepted the object without locking it, a token that fails verification).
	// The record goes to failed.
	ClassPermanent
	// ClassRefused: the target refused the credentials, the authorisation, or
	// the configuration (401, 403, bucket missing, object lock not enabled,
	// TSA rejection). Retried a few times in case of propagation delay
	// (RefusedRetryDelays), then failed.
	ClassRefused
	// ClassConflict: the store already holds a different object under the key
	// of this checkpoint. Failed at once, never retried, never overwritten.
	ClassConflict
)

// String returns the lowercase name of the class.
func (c Class) String() string {
	switch c {
	case ClassRetryable:
		return "retryable"
	case ClassPermanent:
		return "permanent"
	case ClassRefused:
		return "refused"
	case ClassConflict:
		return "conflict"
	}
	return "unknown"
}

// Error is the error type of every anchor target. Its text never contains a
// credential: constructors take only static text, status numbers, and text
// that has passed through a Redactor.
type Error struct {
	Class Class
	// Target is the kind ("worm", "tsa", "ots"), Op the step ("put", "head",
	// "verify-token", ...), Code a stable machine-readable tag such as
	// "not_locked" or "tsa_rejected". Text is for people.
	Target, Op, Code, Text string
	// Status is the HTTP status when there was one (else 0).
	Status int
	// RetryAfter is the delay the server asked for (0 when none).
	RetryAfter time.Duration
	// Final marks a verdict that is the same on every later call (correction 105):
	// an object that carries the start of the call that wrote it and lies outside
	// its window. Record.AfterError fails a record at once on a final error of
	// class refused (the same alert as the fourth refusal) instead of spending
	// three waits on requests that cannot change the answer.
	Final bool
	// Err is the underlying error, for errors.Is and errors.As. It is never
	// printed by Error(); only Text is.
	Err error
}

// Error implements error.
func (e *Error) Error() string {
	s := e.Target
	if e.Op != "" {
		s += " " + e.Op
	}
	s += ": " + e.Text
	if e.Status != 0 {
		s += " (http " + strconv.Itoa(e.Status) + ")"
	}
	return s
}

// Unwrap returns the underlying error.
func (e *Error) Unwrap() error { return e.Err }

// Errorf builds an *Error.
func Errorf(class Class, target, op, code string, format string, args ...any) *Error {
	return &Error{Class: class, Target: target, Op: op, Code: code, Text: fmt.Sprintf(format, args...)}
}

// ClassOf returns the class of err: the class of the first *Error in its
// chain; a context cancellation or deadline is retryable; an error of
// unknown kind is permanent.
func ClassOf(err error) Class {
	var e *Error
	if errors.As(err, &e) {
		return e.Class
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return ClassRetryable
	}
	return ClassPermanent
}

// IsRetryable reports ClassOf(err) == ClassRetryable.
func IsRetryable(err error) bool { return err != nil && ClassOf(err) == ClassRetryable }

// IsPermanent reports ClassOf(err) == ClassPermanent.
func IsPermanent(err error) bool { return err != nil && ClassOf(err) == ClassPermanent }

// IsRefused reports ClassOf(err) == ClassRefused (credentials or access refused).
func IsRefused(err error) bool { return err != nil && ClassOf(err) == ClassRefused }

// IsFinal reports that err is an *Error whose verdict is final (Error.Final).
func IsFinal(err error) bool {
	var e *Error
	return errors.As(err, &e) && e.Final
}

// IsConflict reports ClassOf(err) == ClassConflict ("already there with other content").
func IsConflict(err error) bool { return err != nil && ClassOf(err) == ClassConflict }

// RetryAfterOf returns the delay the server asked for, or 0.
func RetryAfterOf(err error) time.Duration {
	var e *Error
	if errors.As(err, &e) {
		return e.RetryAfter
	}
	return 0
}

// CodeOf returns the machine tag of err, or "".
func CodeOf(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return ""
}

// ClassOfStatus is the default classification of an unexpected HTTP status:
// 401 and 403 refused; 408, 425, 429 and every 5xx retryable; others permanent.
func ClassOfStatus(code int) Class {
	switch {
	case code == http.StatusUnauthorized || code == http.StatusForbidden:
		return ClassRefused
	case code == http.StatusRequestTimeout || code == http.StatusTooEarly || code == http.StatusTooManyRequests || code >= 500:
		return ClassRetryable
	}
	return ClassPermanent
}

// TransportError wraps an error of the HTTP round trip (dial, TLS, timeout,
// cut connection, oversized body) as an *Error. Dial, timeout, cut and caller
// cancellation are retryable. Certificate failures and refused redirects are
// permanent. The text never contains the URL (it can carry a presigned query
// or user information); only the kind of failure.
func TransportError(target, op string, err error) *Error {
	if err == nil {
		return nil
	}
	var e *Error
	if errors.As(err, &e) {
		return e
	}
	class, text := ClassRetryable, "connection failed"
	inner := err
	var ue *url.Error
	if errors.As(err, &ue) {
		inner = ue.Err
	}
	var ne net.Error
	switch {
	case errors.Is(err, ErrRedirectRefused):
		class, text = ClassPermanent, "redirect refused"
	case errors.Is(err, ErrTooLarge):
		class, text = ClassPermanent, "response larger than the limit"
	case errors.Is(err, context.DeadlineExceeded):
		text = "timed out"
	case errors.Is(err, context.Canceled):
		text = "cancelled"
	case errors.Is(err, io.ErrUnexpectedEOF), errors.Is(err, io.EOF):
		text = "connection cut"
	case isCertError(inner):
		class, text = ClassPermanent, "TLS certificate not accepted"
	case errors.As(err, &ne) && ne.Timeout():
		text = "timed out"
	}
	return &Error{Class: class, Target: target, Op: op, Code: "transport", Text: text, Err: err}
}

// RetryAfter parses a Retry-After header (delta seconds or an HTTP date);
// the result is 0 when absent or unparsable and is capped at 24 hours.
func RetryAfter(h http.Header, now time.Time) time.Duration {
	v := h.Get("Retry-After")
	if v == "" {
		return 0
	}
	var d time.Duration
	if n, err := strconv.ParseInt(v, 10, 64); err == nil {
		if n < 0 {
			return 0
		}
		if n > 86400 {
			n = 86400
		}
		d = time.Duration(n) * time.Second
	} else if t, err := http.ParseTime(v); err == nil {
		d = t.Sub(now)
		if d < 0 {
			d = 0
		}
		if d > 24*time.Hour {
			d = 24 * time.Hour
		}
	}
	return d
}
