// SPDX-License-Identifier: AGPL-3.0-only

package worm

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The fake S3 server of the tests. It verifies the SigV4 signature of every
// request with code of its own (stdlib crypto only; nothing from package
// sigv4) and answers as an S3 compatible store with Object Lock, unless told
// to misbehave.

const (
	fakeKeyID  = "AKIAFAKEKEYID000001"
	fakeSecret = "fakeSecretAccessKey/0123456789+abcdefghij"
	fakeToken  = "fakeSessionToken/ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789=="
	fakeRegion = "eu-test-1"
	fakeBucket = "anchors-bucket"
)

type fakeVersion struct {
	id      string
	body    []byte
	mode    string
	retain  string
	lastMod time.Time
	sum     string
}

type fakeReq struct {
	method, uri string
	header      http.Header
	host        string
}

type fakeS3 struct {
	srv *httptest.Server
	mu  sync.Mutex

	objects map[string][]*fakeVersion
	nextID  int
	reqs    []fakeReq
	sigErrs []string

	// behaviour
	needToken      bool
	noLock         bool   // accept the put, store no lock
	forceMode      string // store this mode instead of the requested one
	shortRetain    bool   // store a retain-until one day after the put
	ignoreINM      bool   // overwrite instead of 412
	rejectINM      int    // answer this status when If-None-Match is present
	rejectINMCode  string
	putStatus      int // answer every PUT with this status
	putCode        string
	putMsg         string
	putRetryAfter  string
	headStatus     int // answer HEAD of an object with this status
	getStatus      int
	badSumHeader   bool
	badLength      bool
	badGetBody     bool
	noVersionID    bool
	noLastModified bool
	hang           chan struct{} // when non-nil, PUT waits for it
	cut            bool          // close the connection after the headers of a PUT answer
	redirectTo     string        // answer PUT with 307 to this URL
	bigError       int           // size of the body of error answers
	bigGet         int           // size of the GET body
	versioning     string        // "Enabled" or "Suspended"
	lockEnabled    bool
	defaultMode    string
	defaultDays    int
	echoSecrets    bool // put the credentials in error texts
	failSig        bool
}

func newFakeS3() *fakeS3 {
	f := &fakeS3{objects: map[string][]*fakeVersion{}, versioning: "Enabled", lockEnabled: true}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	return f
}

func (f *fakeS3) close() { f.srv.Close() }

func (f *fakeS3) dialer() *http.Client {
	addr := f.srv.Listener.Addr().String()
	return &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, network, addr)
		},
	}}
}

func (f *fakeS3) stored(key string) []*fakeVersion {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*fakeVersion(nil), f.objects[key]...)
}

func (f *fakeS3) requests() []fakeReq {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]fakeReq(nil), f.reqs...)
}

func (f *fakeS3) signatureErrors() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.sigErrs...)
}

func (f *fakeS3) errXML(w http.ResponseWriter, status int, code, msg string) {
	if f.echoSecrets {
		msg += " key=" + fakeKeyID + " secret=" + fakeSecret + " token=" + fakeToken
	}
	body := `<?xml version="1.0" encoding="UTF-8"?><Error><Code>` + code + `</Code><Message>` + msg + `</Message>`
	if f.echoSecrets {
		body += `<AWSAccessKeyId>` + fakeKeyID + `</AWSAccessKeyId>`
	}
	body += `<RequestId>R1</RequestId></Error>`
	if f.bigError > 0 {
		body += strings.Repeat(" ", f.bigError)
	}
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}

func hm(key []byte, s string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(s))
	return m.Sum(nil)
}

// checkSig is the independent verification: it works from the request line
// exactly as received on the wire.
func (f *fakeS3) checkSig(r *http.Request, body []byte) string {
	auth := r.Header.Get("Authorization")
	const pre = "AWS4-HMAC-SHA256 "
	if !strings.HasPrefix(auth, pre) {
		return "no AWS4-HMAC-SHA256 authorization"
	}
	parts := map[string]string{}
	for _, kv := range strings.Split(strings.TrimPrefix(auth, pre), ", ") {
		k, v, _ := strings.Cut(kv, "=")
		parts[k] = v
	}
	cred := strings.Split(parts["Credential"], "/")
	if len(cred) != 5 || cred[0] != fakeKeyID || cred[2] != fakeRegion || cred[3] != "s3" || cred[4] != "aws4_request" {
		return "bad credential scope " + parts["Credential"]
	}
	amz := r.Header.Get("X-Amz-Date")
	if len(amz) != 16 || amz[:8] != cred[1] {
		return "x-amz-date does not match the credential date"
	}
	ph := r.Header.Get("X-Amz-Content-Sha256")
	sum := sha256.Sum256(body)
	if ph != hex.EncodeToString(sum[:]) {
		return "x-amz-content-sha256 is not the hash of the body"
	}
	if f.needToken && r.Header.Get("X-Amz-Security-Token") != fakeToken {
		return "session token missing or wrong"
	}
	signed := strings.Split(parts["SignedHeaders"], ";")
	if !sort.StringsAreSorted(signed) {
		return "signed headers not sorted"
	}
	need := map[string]bool{"host": false, "x-amz-date": false, "x-amz-content-sha256": false}
	var canonHdrs strings.Builder
	for _, h := range signed {
		if _, ok := need[h]; ok {
			need[h] = true
		}
		var v string
		if h == "host" {
			v = r.Host
		} else {
			v = strings.Join(r.Header.Values(h), ",")
		}
		canonHdrs.WriteString(h + ":" + strings.Join(strings.Fields(v), " ") + "\n")
	}
	for h, ok := range need {
		if !ok {
			return "header " + h + " not signed"
		}
	}
	if r.Method == "PUT" {
		for _, h := range []string{"content-type", "x-amz-checksum-sha256"} {
			if !contains(signed, h) {
				return "header " + h + " not signed on put"
			}
		}
	}
	uri := r.RequestURI
	path, query, _ := strings.Cut(uri, "?")
	var qs []string
	if query != "" {
		for _, p := range strings.Split(query, "&") {
			if !strings.Contains(p, "=") {
				p += "="
			}
			qs = append(qs, p)
		}
		sort.Strings(qs)
	}
	creq := r.Method + "\n" + path + "\n" + strings.Join(qs, "&") + "\n" + canonHdrs.String() + "\n" + parts["SignedHeaders"] + "\n" + ph
	cr := sha256.Sum256([]byte(creq))
	sts := "AWS4-HMAC-SHA256\n" + amz + "\n" + strings.Join(cred[1:], "/") + "\n" + hex.EncodeToString(cr[:])
	k := hm([]byte("AWS4"+fakeSecret), cred[1])
	k = hm(k, cred[2])
	k = hm(k, cred[3])
	k = hm(k, "aws4_request")
	want := hex.EncodeToString(hm(k, sts))
	if !hmac.Equal([]byte(want), []byte(parts["Signature"])) {
		return "signature mismatch for canonical request " + fmt.Sprintf("%q", creq)
	}
	return ""
}

func contains(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

func (f *fakeS3) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reqs = append(f.reqs, fakeReq{r.Method, r.RequestURI, r.Header.Clone(), r.Host})
	if msg := f.checkSig(r, body); msg != "" || f.failSig {
		f.sigErrs = append(f.sigErrs, msg)
		f.errXML(w, 403, "SignatureDoesNotMatch", "The request signature we calculated does not match")
		return
	}
	// resolve bucket and key, path style or virtual host
	path := r.URL.Path
	var key string
	if strings.HasPrefix(r.Host, fakeBucket+".") {
		key = strings.TrimPrefix(path, "/")
	} else if p := strings.TrimPrefix(path, "/"+fakeBucket); p != path {
		key = strings.TrimPrefix(p, "/")
	} else {
		f.errXML(w, 404, "NoSuchBucket", "The specified bucket does not exist")
		return
	}
	q := r.URL.Query()
	if key == "" {
		if _, ok := q["versioning"]; ok {
			fmt.Fprintf(w, `<VersioningConfiguration><Status>%s</Status></VersioningConfiguration>`, f.versioning)
			return
		}
		if _, ok := q["object-lock"]; ok {
			if !f.lockEnabled {
				f.errXML(w, 404, "ObjectLockConfigurationNotFoundError", "Object Lock configuration does not exist for this bucket")
				return
			}
			rule := ""
			if f.defaultMode != "" {
				rule = fmt.Sprintf(`<Rule><DefaultRetention><Mode>%s</Mode><Days>%d</Days></DefaultRetention></Rule>`, f.defaultMode, f.defaultDays)
			}
			fmt.Fprintf(w, `<ObjectLockConfiguration><ObjectLockEnabled>Enabled</ObjectLockEnabled>%s</ObjectLockConfiguration>`, rule)
			return
		}
		f.errXML(w, 400, "InvalidRequest", "unknown bucket operation")
		return
	}
	switch r.Method {
	case "PUT":
		f.put(w, r, key, body)
	case "HEAD", "GET":
		f.read(w, r, key)
	default:
		f.errXML(w, 405, "MethodNotAllowed", "no")
	}
}

func (f *fakeS3) put(w http.ResponseWriter, r *http.Request, key string, body []byte) {
	if f.hang != nil {
		f.mu.Unlock()
		select {
		case <-f.hang:
		case <-r.Context().Done():
		}
		f.mu.Lock()
	}
	if f.redirectTo != "" {
		w.Header().Set("Location", f.redirectTo)
		w.WriteHeader(307)
		return
	}
	if f.cut {
		hj, _ := w.(http.Hijacker)
		c, buf, _ := hj.Hijack()
		_, _ = buf.WriteString("HTTP/1.1 200 OK\r\nContent-Length: 100\r\n\r\npartial")
		_ = buf.Flush()
		_ = c.Close()
		return
	}
	if f.putStatus != 0 {
		if f.putRetryAfter != "" {
			w.Header().Set("Retry-After", f.putRetryAfter)
		}
		f.errXML(w, f.putStatus, f.putCode, f.putMsg)
		return
	}
	inm := r.Header.Get("If-None-Match")
	if inm != "" && f.rejectINM != 0 {
		f.errXML(w, f.rejectINM, f.rejectINMCode, "If-None-Match is not supported")
		return
	}
	if sum := r.Header.Get("X-Amz-Checksum-Sha256"); sum != "" {
		h := sha256.Sum256(body)
		if sum != base64.StdEncoding.EncodeToString(h[:]) {
			f.errXML(w, 400, "BadDigest", "checksum mismatch")
			return
		}
	}
	if inm == "*" && !f.ignoreINM && len(f.objects[key]) > 0 {
		f.errXML(w, 412, "PreconditionFailed", "At least one of the pre-conditions you specified did not hold")
		return
	}
	v := &fakeVersion{body: append([]byte(nil), body...), lastMod: time.Now().UTC().Truncate(time.Second)}
	h := sha256.Sum256(body)
	v.sum = base64.StdEncoding.EncodeToString(h[:])
	f.nextID++
	v.id = "v" + strconv.Itoa(f.nextID) + "-" + hex.EncodeToString(h[:4])
	if mode := r.Header.Get("X-Amz-Object-Lock-Mode"); mode != "" && !f.noLock {
		if !f.lockEnabled {
			f.errXML(w, 400, "InvalidRequest", "Bucket is missing Object Lock Configuration")
			return
		}
		v.mode = mode
		if f.forceMode != "" {
			v.mode = f.forceMode
		}
		v.retain = r.Header.Get("X-Amz-Object-Lock-Retain-Until-Date")
		if t, err := time.Parse(time.RFC3339, v.retain); err != nil {
			f.errXML(w, 400, "InvalidArgument", "bad retain date")
			return
		} else {
			v.retain = t.UTC().Format("2006-01-02T15:04:05.000Z")
		}
		if f.shortRetain {
			v.retain = time.Now().Add(24 * time.Hour).UTC().Format("2006-01-02T15:04:05.000Z")
		}
	}
	f.objects[key] = append(f.objects[key], v)
	if !f.noVersionID {
		w.Header().Set("X-Amz-Version-Id", v.id)
	}
	w.Header().Set("ETag", `"`+v.sum[:8]+`"`)
	w.WriteHeader(200)
}

func (f *fakeS3) read(w http.ResponseWriter, r *http.Request, key string) {
	vs := f.objects[key]
	var v *fakeVersion
	if id := r.URL.Query().Get("versionId"); id != "" {
		for _, x := range vs {
			if x.id == id {
				v = x
			}
		}
	} else if len(vs) > 0 {
		v = vs[len(vs)-1]
	}
	if v == nil {
		if r.Method == "HEAD" {
			w.WriteHeader(404)
			return
		}
		f.errXML(w, 404, "NoSuchKey", "The specified key does not exist.")
		return
	}
	if r.Method == "HEAD" && f.headStatus != 0 {
		w.WriteHeader(f.headStatus)
		return
	}
	if r.Method == "GET" && f.getStatus != 0 {
		f.errXML(w, f.getStatus, "InternalError", "we encountered an internal error")
		return
	}
	body := v.body
	if r.Method == "GET" && f.badGetBody {
		body = append([]byte("X"), body...)
	}
	if r.Method == "GET" && f.bigGet > 0 {
		body = []byte(strings.Repeat("a", f.bigGet))
	}
	if !f.noVersionID {
		w.Header().Set("X-Amz-Version-Id", v.id)
	}
	if v.mode != "" {
		w.Header().Set("X-Amz-Object-Lock-Mode", v.mode)
		w.Header().Set("X-Amz-Object-Lock-Retain-Until-Date", v.retain)
	}
	if !f.noLastModified {
		w.Header().Set("Last-Modified", v.lastMod.Format(http.TimeFormat))
	}
	if r.Header.Get("X-Amz-Checksum-Mode") == "ENABLED" {
		s := v.sum
		if f.badSumHeader {
			s = base64.StdEncoding.EncodeToString(make([]byte, 32))
		}
		w.Header().Set("X-Amz-Checksum-Sha256", s)
	}
	n := len(body)
	if f.badLength && r.Method == "HEAD" {
		n++
	}
	w.Header().Set("ETag", `"`+v.sum[:8]+`"`)
	w.Header().Set("Content-Length", strconv.Itoa(n))
	w.WriteHeader(200)
	if r.Method == "GET" {
		_, _ = w.Write(body)
	}
}
