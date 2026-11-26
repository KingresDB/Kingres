// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package httpapi

import (
	"net/http"
	"time"
)

// sessionCookieName is the name of the session cookie this server sets and
// accepts. It is "kingres_session" with and without TLS: the panel's
// specification (kingres/web/README.md, "What the server must send") fixes the
// name and Path=/. A "__Host-" prefix was considered (the panel never reads the
// HttpOnly cookie, so it would be transparent) and not adopted because the
// agreed contract between the two sides names the plain cookie; see the README,
// "Headers the server sends".
func (s *Server) sessionCookieName() string { return CookieName }

// sessionCookie builds the session cookie, or with maxAge < 0 the cookie that
// clears it (same name, Path and attributes, no Domain, as a browser matches
// cookies by name, domain and path). The cookie is always Secure, as the
// contract states (browsers accept it over plain HTTP on localhost, and
// behind a TLS-terminating proxy it is the proxy's HTTPS that matters).
func (s *Server) sessionCookie(value string, expires time.Time, maxAge int) *http.Cookie {
	c := &http.Cookie{
		Name: s.sessionCookieName(), Value: value, Path: "/",
		HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode,
	}
	if maxAge < 0 {
		c.MaxAge = -1
		return c
	}
	c.Expires, c.MaxAge = expires, maxAge
	return c
}
