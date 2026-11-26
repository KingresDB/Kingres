// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package sigv4

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	suiteKeyID  = "AKIDEXAMPLE"
	suiteSecret = "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY"
)

func readFile(t *testing.T, p string) string {
	t.Helper()
	if strings.Contains(p, "third_party") {
		needThirdParty(t)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimRight(strings.ReplaceAll(string(b), "\r\n", "\n"), "\n")
}

type suiteReq struct {
	method, path, query string
	headers             []Header
	body                string
}

func parseReq(t *testing.T, text string) suiteReq {
	t.Helper()
	text = strings.ReplaceAll(text, "\r\n", "\n")
	head, body, _ := strings.Cut(text, "\n\n")
	lines := strings.Split(head, "\n")
	first := strings.TrimSuffix(lines[0], " HTTP/1.1")
	method, target, _ := strings.Cut(first, " ")
	r := suiteReq{method: method, body: body}
	r.path, r.query, _ = strings.Cut(target, "?")
	for _, l := range lines[1:] {
		if l != "" && (l[0] == ' ' || l[0] == '\t') {
			r.headers[len(r.headers)-1].Value += "\n" + l
			continue
		}
		n, v, _ := strings.Cut(l, ":")
		r.headers = append(r.headers, Header{n, v})
	}
	return r
}

// TestAWSSuite runs the cases of aws-sig-v4-test-suite (see testdata/SOURCES.txt):
// canonical request, string to sign and Authorization header of each.
func TestAWSSuite(t *testing.T) {
	needThirdParty(t)
	ents, err := os.ReadDir("testdata/third_party/aws-sig-v4-test-suite")
	if err != nil {
		t.Fatal(err)
	}
	var dirs []string
	for _, e := range ents {
		if e.IsDir() {
			dirs = append(dirs, filepath.Join("testdata/third_party/aws-sig-v4-test-suite", e.Name()))
		}
	}
	if len(dirs) < 25 {
		t.Fatalf("only %d cases found", len(dirs))
	}
	for _, d := range dirs {
		name := filepath.Base(d)
		t.Run(name, func(t *testing.T) {
			base := filepath.Join(d, name)
			r := parseReq(t, readFile(t, base+".req"))
			if strings.Contains(name, "session-token") {
				// the .req of this case lacks the token that the .sreq carries
				sr := parseReq(t, readFile(t, base+".sreq"))
				r.headers = nil
				for _, h := range sr.headers {
					if h.Name != "Authorization" {
						r.headers = append(r.headers, h)
					}
				}
			}
			var amz string
			for _, h := range r.headers {
				if strings.EqualFold(h.Name, "x-amz-date") {
					amz = h.Value
				}
			}
			at, err := time.Parse(AmzDateLayout, amz)
			if err != nil {
				t.Fatal(err)
			}
			in := Input{Method: r.method, Path: r.path, RawQuery: r.query, Headers: r.headers, PayloadHash: HashPayload([]byte(r.body))}
			creq, signed := Canonical(in, true)
			if want := readFile(t, base+".creq"); creq != want {
				t.Fatalf("canonical request\n got: %q\nwant: %q", creq, want)
			}
			scope := Scope(at, "us-east-1", "service")
			sts := StringToSign(at, scope, creq)
			if want := readFile(t, base+".sts"); sts != want {
				t.Fatalf("string to sign\n got: %q\nwant: %q", sts, want)
			}
			sig := Signature(SigningKey(suiteSecret, at, "us-east-1", "service"), sts)
			got := Authorization(suiteKeyID, scope, signed, sig)
			if want := readFile(t, base+".authz"); got != want {
				t.Fatalf("authorization\n got: %q\nwant: %q", got, want)
			}
		})
	}
}

// The examples of the S3 documentation "Authenticating Requests: Using the
// Authorization Header (AWS Signature Version 4)", signing with Sign on a real
// http.Request (S3 rules: no path normalisation).
func TestS3DocumentationExamples(t *testing.T) {
	creds := Credentials{AccessKeyID: "AKIAIOSFODNN7EXAMPLE", SecretAccessKey: "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"}
	at := time.Date(2013, 5, 24, 0, 0, 0, 0, time.UTC)
	tests := []struct {
		name, method, url, body string
		hdr                     map[string]string
		sig, signed             string
	}{
		{"get-object", "GET", "https://examplebucket.s3.amazonaws.com/test.txt", "",
			map[string]string{"Range": "bytes=0-9"},
			"f0e8bdb87c964420e857bd35b5d6ed310bd44f0170aba48dd91039c6036bdb41",
			"host;range;x-amz-content-sha256;x-amz-date"},
		{"put-object", "PUT", "https://examplebucket.s3.amazonaws.com/test%24file.text", "Welcome to Amazon S3.",
			map[string]string{"Date": "Fri, 24 May 2013 00:00:00 GMT", "X-Amz-Storage-Class": "REDUCED_REDUNDANCY"},
			"98ad721746da40c64f1a55b78f14c238d841ea1380cd77a1b5971af0ece108bd",
			"date;host;x-amz-content-sha256;x-amz-date;x-amz-storage-class"},
		{"get-lifecycle", "GET", "https://examplebucket.s3.amazonaws.com/?lifecycle", "", nil,
			"fea454ca298b7da1c68078a5d1bdbfbbe0d65c699e0f91ac7a200a0136783543",
			"host;x-amz-content-sha256;x-amz-date"},
		{"list-objects", "GET", "https://examplebucket.s3.amazonaws.com/?max-keys=2&prefix=J", "", nil,
			"34b48302e7b5fa45bde8084f4b7868a86f0a534bc59db6670ed5711ef69dc6f7",
			"host;x-amz-content-sha256;x-amz-date"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req, err := http.NewRequest(tc.method, tc.url, nil)
			if err != nil {
				t.Fatal(err)
			}
			for k, v := range tc.hdr {
				req.Header.Set(k, v)
			}
			s := Signer{Credentials: creds, Region: "us-east-1", Service: "s3"}
			if err := s.Sign(req, HashPayload([]byte(tc.body)), at); err != nil {
				t.Fatal(err)
			}
			want := "AWS4-HMAC-SHA256 Credential=AKIAIOSFODNN7EXAMPLE/20130524/us-east-1/s3/aws4_request,SignedHeaders=" + tc.signed + ",Signature=" + tc.sig
			want = strings.Replace(want, ",", ", ", 2)
			if got := req.Header.Get("Authorization"); got != want {
				t.Fatalf("got  %s\nwant %s", got, want)
			}
		})
	}
}

func TestSignMatchesSuiteVanilla(t *testing.T) {
	req, _ := http.NewRequest("GET", "http://example.amazonaws.com/", nil)
	s := Signer{Credentials: Credentials{AccessKeyID: suiteKeyID, SecretAccessKey: suiteSecret}, Region: "us-east-1", Service: "service", NormalizePath: true, OmitContentSHA256Header: true}
	if err := s.Sign(req, EmptyPayloadHash, time.Date(2015, 8, 30, 12, 36, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	want := readFile(t, "testdata/third_party/aws-sig-v4-test-suite/get-vanilla/get-vanilla.authz")
	if got := req.Header.Get("Authorization"); got != want {
		t.Fatalf("got %s", got)
	}
}

func TestSignSessionToken(t *testing.T) {
	at := time.Date(2015, 8, 30, 12, 36, 0, 0, time.UTC)
	long := "AQoDYXdzEPT//////////wEXAMPLEtc764bNrC9SAPBSM22wDOk4x4HIZ8j4FZTwdQWLWsKWHGBuFqwAeMicRXmxfpSPfIeoIYRqTflfKD8YUuwthAx7mSEI/qkPpKPi/kMcGdQrmGdeehM4IC1NtBmUpp2wUE8phUZampKsburEDy0KPkyQDYwT7WZ0wq5VSXDvp75YU9HFvlRd8Tx6q6fE8YQcHNVXAkiY9q6d+xo0rKwT38xVqr7ZD0u0iPPkUL64lIZbqBAz+scqKmlzm8FDrypNC9Yjc8fPOLn9FX9KSYvKTr4rvx3iSIlTJabIQwj2ICCR/oLxBA=="
	for _, tc := range []struct{ method, token, authz string }{
		{"GET", "6e86291e8372ff2a2260956d9b8aae1d763fbf315fa00fa31553b73ebf194267", "testdata/third_party/aws-sig-v4-test-suite/get-vanilla-with-session-token/get-vanilla-with-session-token.authz"},
		{"POST", long, "testdata/third_party/aws-sig-v4-test-suite/post-sts-header-before/post-sts-header-before.authz"},
	} {
		req, _ := http.NewRequest(tc.method, "http://example.amazonaws.com/", nil)
		s := Signer{Credentials: Credentials{AccessKeyID: suiteKeyID, SecretAccessKey: suiteSecret, SessionToken: anchorSecret(tc.token)},
			Region: "us-east-1", Service: "service", NormalizePath: true, OmitContentSHA256Header: true}
		if err := s.Sign(req, EmptyPayloadHash, at); err != nil {
			t.Fatal(err)
		}
		want := readFile(t, tc.authz)
		if got := req.Header.Get("Authorization"); got != want {
			t.Fatalf("got %s\nwant %s", got, want)
		}
		if req.Header.Get("X-Amz-Security-Token") != tc.token {
			t.Fatal("token header missing")
		}
	}
}

func TestCanonicalAwkward(t *testing.T) {
	h := func(kv ...string) []Header {
		var r []Header
		for i := 0; i < len(kv); i += 2 {
			r = append(r, Header{kv[i], kv[i+1]})
		}
		return r
	}
	tests := []struct {
		name  string
		in    Input
		norm  bool
		lines map[int]string // line index of the canonical request -> content
	}{
		{"empty-value-and-empty-key", Input{Method: "GET", Path: "/", RawQuery: "a=&=b&&c", PayloadHash: "H"}, false, map[int]string{2: "=b&a=&c="}},
		{"repeated-key-sorted-by-value", Input{Method: "GET", Path: "/", RawQuery: "k=b&k=a&k=B&k=", PayloadHash: "H"}, false, map[int]string{2: "k=&k=B&k=a&k=b"}},
		{"plus-is-space", Input{Method: "GET", Path: "/", RawQuery: "a=b+c&d=e%2Bf", PayloadHash: "H"}, false, map[int]string{2: "a=b%20c&d=e%2Bf"}},
		{"slash-and-equals-in-value", Input{Method: "GET", Path: "/", RawQuery: "p=a/b=c", PayloadHash: "H"}, false, map[int]string{2: "p=a%2Fb%3Dc"}},
		{"reserved-chars-encoded", Input{Method: "GET", Path: "/", RawQuery: "k=!*'()$,;:@", PayloadHash: "H"}, false, map[int]string{2: "k=%21%2A%27%28%29%24%2C%3B%3A%40"}},
		{"tilde-kept", Input{Method: "GET", Path: "/~a", RawQuery: "k=~", PayloadHash: "H"}, false, map[int]string{1: "/~a", 2: "k=~"}},
		{"utf8-path", Input{Method: "GET", Path: "/\xc3\xa9 x", PayloadHash: "H"}, false, map[int]string{1: "/%C3%A9%20x"}},
		{"s3-keeps-dots-and-double-slashes", Input{Method: "GET", Path: "/a//b/../c/./", PayloadHash: "H"}, false, map[int]string{1: "/a//b/../c/./"}},
		{"generic-removes-dots-and-double-slashes", Input{Method: "GET", Path: "/a//b/../c/./", PayloadHash: "H"}, true, map[int]string{1: "/a/c/"}},
		{"dotdot-above-root", Input{Method: "GET", Path: "/../../a", PayloadHash: "H"}, true, map[int]string{1: "/a"}},
		{"percent-in-path-is-encoded-once", Input{Method: "GET", Path: "/100%", PayloadHash: "H"}, false, map[int]string{1: "/100%25"}},
		{"header-fold", Input{Method: "GET", Path: "/", Headers: h("Host", "h", "X-A", "  a \t  b   c  "), PayloadHash: "H"}, false, map[int]string{3: "host:h", 4: "x-a:a b c"}},
		{"header-repeat-keeps-order", Input{Method: "GET", Path: "/", Headers: h("Host", "h", "X-A", "2", "x-a", "1"), PayloadHash: "H"}, false, map[int]string{4: "x-a:2,1"}},
		{"header-empty-value", Input{Method: "GET", Path: "/", Headers: h("Host", "h", "X-A", ""), PayloadHash: "H"}, false, map[int]string{4: "x-a:"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			creq, _ := Canonical(tc.in, tc.norm)
			lines := strings.Split(creq, "\n")
			for i, want := range tc.lines {
				if lines[i] != want {
					t.Fatalf("line %d: got %q want %q\n%s", i, lines[i], want, creq)
				}
			}
		})
	}
}

func TestSignRejectsBadInput(t *testing.T) {
	req, _ := http.NewRequest("GET", "http://h/", nil)
	if err := (Signer{Region: "r", Service: "s3"}).Sign(req, "x", time.Now()); err == nil {
		t.Fatal("empty credentials accepted")
	}
	c := Credentials{AccessKeyID: "a", SecretAccessKey: "b"}
	if err := (Signer{Credentials: c, Service: "s3"}).Sign(req, "x", time.Now()); err == nil {
		t.Fatal("empty region accepted")
	}
	if err := (Signer{Credentials: c, Region: "r", Service: "s3"}).Sign(req, "", time.Now()); err == nil {
		t.Fatal("empty payload hash accepted")
	}
}

func TestSignIgnoresStackManagedHeaders(t *testing.T) {
	req, _ := http.NewRequest("PUT", "http://h:9000/b/k", nil)
	req.Header.Set("User-Agent", "x")
	req.Header.Set("Accept-Encoding", "gzip")
	req.Header.Set("If-None-Match", "*")
	s := Signer{Credentials: Credentials{AccessKeyID: "a", SecretAccessKey: "b"}, Region: "r", Service: "s3"}
	if err := s.Sign(req, EmptyPayloadHash, time.Unix(0, 0)); err != nil {
		t.Fatal(err)
	}
	a := req.Header.Get("Authorization")
	if !strings.Contains(a, "SignedHeaders=host;if-none-match;x-amz-content-sha256;x-amz-date,") {
		t.Fatal(a)
	}
}

func TestNoSecretInFormatting(t *testing.T) {
	const keyID, secret, token = "AKIASECRETKEYID1234", "SECRETSECRETSECRET1234567890", "TOKENTOKENTOKEN1234567890"
	s := Signer{Credentials: Credentials{AccessKeyID: anchorSecret(keyID), SecretAccessKey: anchorSecret(secret), SessionToken: anchorSecret(token)}, Region: "r", Service: "s3"}
	var all []string
	for _, v := range []any{s, &s, s.Credentials, &s.Credentials} {
		for _, f := range []string{"%v", "%+v", "%#v", "%s", "%q", "%x", "%d"} {
			all = append(all, fmt.Sprintf(f, v))
		}
		j, _ := json.Marshal(v)
		all = append(all, string(j))
	}
	for _, o := range all {
		for _, sec := range []string{keyID, secret, token} {
			if strings.Contains(o, sec) {
				t.Fatalf("secret %q leaked in %q", sec, o)
			}
		}
	}
	// an error from Sign
	req, _ := http.NewRequest("GET", "http://h/", nil)
	err := (Signer{Credentials: s.Credentials}).Sign(req, "x", time.Now())
	for _, sec := range []string{keyID, secret, token} {
		if strings.Contains(err.Error(), sec) {
			t.Fatal("leak in error")
		}
	}
}
