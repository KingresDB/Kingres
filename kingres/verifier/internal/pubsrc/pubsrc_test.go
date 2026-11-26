// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package pubsrc

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/verifier/internal/safefile"
	"github.com/KingresDB/Kingres/kingres/verifier/internal/testdir"
)

type tent struct {
	name string
	typ  byte
	body string
	link string
}

func mkTar(t *testing.T, ents ...tent) string {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	for _, e := range ents {
		h := &tar.Header{Name: e.name, Typeflag: e.typ, Mode: 0o644, Size: int64(len(e.body)), Linkname: e.link}
		if e.typ != tar.TypeReg {
			h.Size = 0
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if e.typ == tar.TypeReg {
			_, _ = tw.Write([]byte(e.body))
		}
	}
	_ = tw.Close()
	_ = zw.Close()
	p := filepath.Join(testdir.New(t), "b.tar.gz")
	if err := os.WriteFile(p, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestTarReadsFilesAndDirectories(t *testing.T) {
	p := mkTar(t, tent{name: "./", typ: tar.TypeDir}, tent{name: "./c/", typ: tar.TypeDir}, tent{name: "./c/x/checkpoint", typ: tar.TypeReg, body: "hello"}, tent{name: "bundle.json", typ: tar.TypeReg, body: "{}"})
	s, err := Open(p, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	b, err := s.ReadFile("c/x/checkpoint", 100)
	if err != nil || string(b) != "hello" {
		t.Fatalf("%q %v", b, err)
	}
	if _, err := s.ReadFile("c/x/checkpoint", 2); !errors.Is(err, safefile.ErrTooLarge) {
		t.Fatalf("limit: %v", err)
	}
	if _, err := s.ReadFile("nope", 10); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("%v", err)
	}
	if _, err := s.ReadFile("c", 10); !errors.Is(err, safefile.ErrNotRegular) {
		t.Fatalf("a directory is not a file: %v", err)
	}
	d, _, err := s.SubDirs("c")
	if err != nil || len(d) != 1 || d[0] != "x" {
		t.Fatalf("%v %v", d, err)
	}
	fs1, err := s.Files("c/x")
	if err != nil || len(fs1) != 1 || fs1[0] != "checkpoint" {
		t.Fatalf("%v %v", fs1, err)
	}
	if _, err := s.ReadFile("../x", 10); err == nil {
		t.Fatal("a path out of the archive was read")
	}
}

func TestTarRefusesWhatAPublicationNeverHolds(t *testing.T) {
	cases := map[string][]tent{
		"parent element":    {{name: "../evil", typ: tar.TypeReg, body: "x"}},
		"nested parent":     {{name: "a/../../evil", typ: tar.TypeReg, body: "x"}},
		"absolute":          {{name: "/etc/passwd", typ: tar.TypeReg, body: "x"}},
		"backslash":         {{name: `a\b`, typ: tar.TypeReg, body: "x"}},
		"colon":             {{name: "C:evil", typ: tar.TypeReg, body: "x"}},
		"symlink":           {{name: "l", typ: tar.TypeSymlink, link: "/etc/passwd"}},
		"hard link":         {{name: "l", typ: tar.TypeLink, link: "x"}},
		"fifo":              {{name: "f", typ: tar.TypeFifo}},
		"char device":       {{name: "d", typ: tar.TypeChar}},
		"twice":             {{name: "a", typ: tar.TypeReg, body: "1"}, {name: "a", typ: tar.TypeReg, body: "2"}},
		"file and dir":      {{name: "a/b", typ: tar.TypeReg, body: "1"}, {name: "a", typ: tar.TypeReg, body: "2"}},
		"dir over a file":   {{name: "a", typ: tar.TypeReg, body: "1"}, {name: "a", typ: tar.TypeDir}},
		"too deep":          {{name: strings.Repeat("d/", 20) + "f", typ: tar.TypeReg, body: "1"}},
		"empty name":        {{name: "", typ: tar.TypeReg, body: "1"}},
		"dot dot directory": {{name: "..", typ: tar.TypeDir}},
	}
	for name, ents := range cases {
		if _, err := OpenTar(mkTar(t, ents...)); err == nil {
			t.Errorf("%s: the archive was accepted", name)
		}
	}
}

func TestTarLimitsAreAppliedWhileReading(t *testing.T) {
	oldF, oldT, oldM := MaxArchiveFiles, MaxArchiveTotal, MaxArchiveMember
	defer func() { MaxArchiveFiles, MaxArchiveTotal, MaxArchiveMember = oldF, oldT, oldM }()
	big := tent{name: "big", typ: tar.TypeReg, body: strings.Repeat("a", 5000)}
	MaxArchiveMember = 1000
	if _, err := OpenTar(mkTar(t, big)); !errors.Is(err, safefile.ErrTooLarge) {
		t.Fatalf("one file over its limit: %v", err)
	}
	MaxArchiveMember, MaxArchiveTotal = 1<<20, 6000
	if _, err := OpenTar(mkTar(t, big, tent{name: "big2", typ: tar.TypeReg, body: strings.Repeat("b", 5000)})); !errors.Is(err, safefile.ErrTooLarge) {
		t.Fatalf("total over its limit: %v", err)
	}
	MaxArchiveTotal, MaxArchiveFiles = 1<<20, 3
	var many []tent
	for i := 0; i < 10; i++ {
		many = append(many, tent{name: string(rune('a'+i)) + "/f", typ: tar.TypeReg, body: "x"})
	}
	if _, err := OpenTar(mkTar(t, many...)); !errors.Is(err, safefile.ErrTooLarge) {
		t.Fatalf("too many entries: %v", err)
	}
	// the file itself
	MaxArchiveFiles = oldF
	old := MaxArchiveFile
	MaxArchiveFile = 10
	defer func() { MaxArchiveFile = old }()
	if _, err := OpenTar(mkTar(t, big)); err == nil {
		t.Fatal("an archive file over its limit was read")
	}
}

func TestTarRefusesTrailingDataAndASecondMember(t *testing.T) {
	p := mkTar(t, tent{name: "a", typ: tar.TypeReg, body: "1"})
	b, _ := os.ReadFile(p)
	var second bytes.Buffer
	zw := gzip.NewWriter(&second)
	_, _ = zw.Write([]byte("more"))
	_ = zw.Close()
	if err := os.WriteFile(p, append(b, second.Bytes()...), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenTar(p); err == nil {
		t.Fatal("a second gzip member was accepted")
	}
	if err := os.WriteFile(p, []byte("not gzip at all"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenTar(p); err == nil {
		t.Fatal("a file that is not gzip was accepted")
	}
}

func TestOpenChoosesByTheNameAndRefusesOtherSchemes(t *testing.T) {
	d := testdir.New(t)
	s, err := Open(d, Options{})
	if err != nil || s.Kind() != "directory" {
		t.Fatalf("%v", err)
	}
	s.Close()
	for _, bad := range []string{"", "ftp://x/y", "file:///etc", "gopher://x"} {
		if _, err := Open(bad, Options{}); err == nil {
			t.Errorf("%q was accepted", bad)
		}
	}
}

func TestHTTPBaseAddressRules(t *testing.T) {
	good := []string{"https://example.com/kingres", "https://example.com:8443/a/b/", "http://127.0.0.1:1234", "http://localhost/x", "http://[::1]:80/x"}
	for _, g := range good {
		s, err := OpenHTTP(g, HTTPOptions{})
		if err != nil {
			t.Errorf("%s: %v", g, err)
			continue
		}
		s.Close()
	}
	bad := map[string]string{
		"http://example.com/x":        "plain http",
		"http://10.0.0.1/x":           "plain http",
		"http://127.0.0.1.evil.com/x": "plain http",
		"https://u:p@example.com/":    "user name",
		"https://example.com/?a=1":    "query",
		"https://example.com/#f":      "query",
		"https:///x":                  "host",
		"https://example.com/a%2fb":   "percent",
	}
	for u, want := range bad {
		_, err := OpenHTTP(u, HTTPOptions{})
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want %q", u, err, want)
		}
	}
}

func TestHTTPStatusesAndLimits(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ok":
			_, _ = w.Write([]byte("0123456789"))
		case "/redirect":
			http.Redirect(w, r, "/ok", http.StatusFound)
		case "/err":
			http.Error(w, "x", 500)
		case "/busy":
			http.Error(w, "x", 429)
		case "/teapot":
			http.Error(w, "x", 418)
		case "/slow":
			time.Sleep(300 * time.Millisecond)
			_, _ = w.Write([]byte("late"))
		case "/endless":
			w.Header().Set("Content-Type", "application/octet-stream")
			for i := 0; i < 1000; i++ {
				_, _ = w.Write(bytes.Repeat([]byte("a"), 1024))
			}
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	s, err := OpenHTTP(srv.URL, HTTPOptions{Request: 150 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if b, err := s.ReadFile("ok", 100); err != nil || string(b) != "0123456789" {
		t.Fatalf("%q %v", b, err)
	}
	if _, err := s.ReadFile("ok", 5); !errors.Is(err, safefile.ErrTooLarge) {
		t.Fatalf("limit: %v", err)
	}
	if _, err := s.ReadFile("missing", 5); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("404: %v", err)
	}
	if _, err := s.ReadFile("redirect", 100); !errors.Is(err, ErrRedirect) {
		t.Fatalf("a redirect was followed or hidden: %v", err)
	}
	for _, n := range []string{"err", "busy", "slow"} {
		if _, err := s.ReadFile(n, 100); !errors.Is(err, ErrUnreachable) {
			t.Fatalf("%s: %v", n, err)
		}
	}
	if _, err := s.ReadFile("teapot", 100); err == nil || errors.Is(err, ErrUnreachable) || errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("418: %v", err)
	}
	if _, err := s.ReadFile("endless", 4096); !errors.Is(err, safefile.ErrTooLarge) {
		t.Fatalf("endless body: %v", err)
	}
	if _, err := s.ReadFile("../etc", 5); err == nil {
		t.Fatal("a path with .. was requested")
	}
	if _, _, err := s.SubDirs("c"); !errors.Is(err, ErrNoListing) {
		t.Fatalf("%v", err)
	}
	if _, err := s.Files("c"); !errors.Is(err, ErrNoListing) {
		t.Fatalf("%v", err)
	}
}

func TestHTTPNeverUsesAProxyFromTheEnvironment(t *testing.T) {
	if s, err := OpenHTTP("https://example.com", HTTPOptions{}); err != nil || s.(*httpSource).client.Transport.(*http.Transport).Proxy != nil {
		t.Fatal("the transport has a proxy function: a proxy from the environment would be used for any address that is not this machine")
	}
	hit := false
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hit = true }))
	defer proxy.Close()
	t.Setenv("HTTP_PROXY", proxy.URL)
	t.Setenv("http_proxy", proxy.URL)
	t.Setenv("NO_PROXY", "")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("direct")) }))
	defer srv.Close()
	s, _ := OpenHTTP(srv.URL, HTTPOptions{})
	b, err := s.ReadFile("x", 100)
	if err != nil || string(b) != "direct" || hit {
		t.Fatalf("%q %v proxy hit %v", b, err, hit)
	}
}

func FuzzOpenTar(f *testing.F) {
	f.Add([]byte("not a tar"))
	f.Fuzz(func(t *testing.T, b []byte) {
		// whatever the bytes are, reading them as an archive must neither panic nor use more than the limits allow
		old := MaxArchiveTotal
		MaxArchiveTotal = 1 << 20
		defer func() { MaxArchiveTotal = old }()
		p := filepath.Join(t.TempDir(), "f.tar.gz")
		var buf bytes.Buffer
		zw := gzip.NewWriter(&buf)
		_, _ = zw.Write(b)
		_ = zw.Close()
		_ = os.WriteFile(p, buf.Bytes(), 0o600)
		if s, err := OpenTar(p); err == nil {
			_, _ = s.ReadFile("a", 10)
			_, _, _ = s.SubDirs("")
		}
		_ = os.WriteFile(p, b, 0o600)
		_, _ = OpenTar(p)
	})
}
