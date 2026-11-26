// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

// Package pubsrc reads a publication (ledger-phase7.md 4.6) from a directory, a .tar.gz file or an
// HTTP(S) base URL through one interface, so that every command reads the three the same way.
// Everything read is bounded and untrusted:
//
//   - a directory is read through safefile.Dir (no link, no special file, no path out of it);
//   - a .tar.gz is read once into memory under a count and a size limit; a name that is not a
//     plain relative path, a link, a device, a directory entry that is also a file, a repeated name
//     is refused, so nothing in an archive can name a place outside it;
//   - an HTTP source never follows a redirect (tlog-tiles: no redirects), never uses a proxy from
//     the environment, has a timeout on the connection, the headers and the whole request, and
//     reads at most the limit of the caller.
package pubsrc

import (
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/url"
	"strings"

	"github.com/KingresDB/Kingres/kingres/verifier/internal/safefile"
)

// Source is a publication.
type Source interface {
	// ReadFile reads the regular file rel (slash separated, built from numbers and hex by the
	// caller). A file that is not there wraps fs.ErrNotExist, one that must not be used wraps
	// what safefile.Unusable recognises, a source that cannot be reached wraps ErrUnreachable.
	ReadFile(rel string, limit int64) ([]byte, error)
	// SubDirs lists the subdirectories of rel, and the names of entries that are neither
	// directories nor regular files. An HTTP source cannot list and returns ErrNoListing.
	SubDirs(rel string) (dirs, others []string, err error)
	// Files lists the regular files directly in the directory rel. An HTTP source cannot list.
	Files(rel string) ([]string, error)
	// Close releases the source.
	Close() error
	// Kind is "directory", "tar.gz" or "http".
	Kind() string
}

// Errors of the sources.
var (
	// ErrUnreachable: the network, a timeout or the server (5xx, 429) did not give the file. Nothing
	// is known about the file; the callers treat it as "could not check" (status 3), never as a failure of the log.
	ErrUnreachable = errors.New("the publication could not be reached")
	// ErrNoListing: an HTTP source has no directory listing.
	ErrNoListing = errors.New("an HTTP source cannot list directories")
	// ErrRedirect: the server answered with a redirect, which is never followed.
	ErrRedirect = errors.New("the server answered with a redirect, which is not followed (tlog-tiles: no redirects); give the address that serves the files")
)

// Options of Open.
type Options struct {
	// HTTP is the configuration of an HTTP source; the zero value is the default.
	HTTP HTTPOptions
}

// Open opens the source named on the command line: a URL with the scheme https (or http for
// an address of this machine), a file ending in .tar.gz or .tgz, or a directory.
func Open(spec string, opt Options) (Source, error) {
	if spec == "" {
		return nil, errors.New("the source is empty")
	}
	if strings.HasPrefix(spec, "https://") || strings.HasPrefix(spec, "http://") {
		return OpenHTTP(spec, opt.HTTP)
	}
	if strings.Contains(spec, "://") {
		return nil, fmt.Errorf("the source %q has a scheme that is not https (or http for this machine), expected a directory, a .tar.gz file or an https URL", spec)
	}
	if strings.HasSuffix(spec, ".tar.gz") || strings.HasSuffix(spec, ".tgz") {
		return OpenTar(spec)
	}
	return OpenDir(spec)
}

// dirSource is a directory.
type dirSource struct{ d *safefile.Dir }

// OpenDir opens a directory source.
func OpenDir(path string) (Source, error) {
	d, err := safefile.OpenDir(path)
	if err != nil {
		return nil, err
	}
	return dirSource{d}, nil
}

func (s dirSource) ReadFile(rel string, limit int64) ([]byte, error) { return s.d.ReadFile(rel, limit) }
func (s dirSource) SubDirs(rel string) ([]string, []string, error)   { return s.d.SubDirs(rel) }
func (s dirSource) Files(rel string) ([]string, error)               { return s.d.RegularFiles(rel) }
func (s dirSource) Close() error                                     { return s.d.Close() }
func (s dirSource) Kind() string                                     { return "directory" }

// loopback reports whether host names this machine: the literal address of a loopback interface or
// the name localhost. A name is not resolved to decide this.
func loopback(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// parseBase checks a base URL and returns it without a trailing slash.
func parseBase(spec string) (*url.URL, error) {
	u, err := url.Parse(spec)
	if err != nil {
		return nil, fmt.Errorf("the source URL is not valid: %w", err)
	}
	switch {
	case u.Host == "" || u.Hostname() == "":
		return nil, errors.New("the source URL has no host")
	case u.User != nil:
		return nil, errors.New("the source URL carries a user name or password, which is refused: it would be sent to whoever the address names")
	case u.RawQuery != "" || u.Fragment != "" || strings.Contains(spec, "?") || strings.Contains(spec, "#"):
		return nil, errors.New("the source URL has a query or a fragment, expected a base address")
	case u.Scheme == "http" && !loopback(u.Hostname()):
		return nil, errors.New("plain http is accepted only for an address of this machine (127.0.0.1, ::1, localhost); use https for anything else")
	case u.Scheme != "http" && u.Scheme != "https":
		return nil, errors.New("the source URL must be https")
	}
	u.Path = strings.TrimRight(u.Path, "/")
	u.RawPath = ""
	if strings.ContainsAny(u.Path, "\\") || strings.Contains(spec, "%") {
		return nil, errors.New("the source URL path has a backslash or a percent sign")
	}
	return u, nil
}

var _ = fs.ErrNotExist
