// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package internalapi

import (
	"errors"
	"net"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

var hostPortRE = regexp.MustCompile(`^(\[[0-9a-fA-F:]+\]|[A-Za-z0-9.-]+):([1-9][0-9]{0,4})$`)

// URL returns the https URL of path (below Prefix) on the internal API at
// addr. It is the only place where such a URL is built: addr must be a plain
// host:port (no user information, path, query or fragment) and path may hold
// neither a query nor a fragment, so no part can change the authority or the
// target of the request.
func URL(addr, path string) (string, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil || !hostPortRE.MatchString(addr) {
		return "", errors.New("the internal address is not a host:port")
	}
	if n, perr := strconv.Atoi(port); perr != nil || n > 65535 {
		return "", errors.New("the internal address has no usable port")
	}
	if !strings.HasPrefix(path, "/") || strings.ContainsAny(path, "?#\\\r\n") || strings.Contains(path, "..") {
		return "", errors.New("the internal path is not valid")
	}
	u := url.URL{Scheme: "https", Host: net.JoinHostPort(host, port), Path: Prefix + path}
	return u.String(), nil
}
