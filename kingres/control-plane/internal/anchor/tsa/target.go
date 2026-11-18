// SPDX-License-Identifier: AGPL-3.0-only

package tsa

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/anchor"
)

// Defaults and limits of the configuration.
const (
	DefaultTimeoutSeconds = 20
	MaxTimeoutSeconds     = 300
	MaxBundleCerts        = 64
	MaxMaxSkewSeconds     = 86400
	maxRedirects          = 3
)

// Config is the non-secret configuration of a TSA target (design 6.4).
type Config struct {
	URL            string `json:"url"`
	HashAlgorithm  string `json:"hashAlgorithm,omitempty"` // "" or "sha256"
	PolicyOID      string `json:"policyOid,omitempty"`
	CABundlePath   string `json:"caBundlePath"` // read by the caller, not by this package
	TimeoutSeconds int    `json:"timeoutSeconds,omitempty"`
	// MaxSkewSeconds is the tolerance between genTime and the clock of
	// Submit, both ways: a token dated further off is refused (code
	// "time_skew"). 0 means 300; the range is 1 to 86400.
	MaxSkewSeconds int `json:"maxSkewSeconds,omitempty"`
	// AllowInsecure permits HTTP Basic credentials over an http:// URL (the
	// token is self-authenticating, the credentials are not). Without it a
	// target with a username and an http URL is refused.
	AllowInsecure bool `json:"allowInsecure,omitempty"`
	// SignerKeySHA256 optionally pins the signer: 64 lower case hexadecimal
	// digits, the SHA-256 of the DER SubjectPublicKeyInfo of the signing
	// certificate (SignerKeyHash). With it a token of any other signing key is
	// refused (code "signer_not_pinned") even when it chains to the roots; without
	// it any authority under the configured roots is accepted, and the name in
	// the token is only checked against the token's own certificate.
	SignerKeySHA256 string `json:"signerKeySha256,omitempty"`
}

// ParseConfig parses and validates the JSON configuration. Unknown members
// are an error. No file is read and no network is used.
func ParseConfig(cfg json.RawMessage) (Config, error) {
	var c Config
	dec := json.NewDecoder(bytes.NewReader(cfg))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return c, fmt.Errorf("tsa: config: %w", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return c, errors.New("tsa: config: trailing data")
	}
	u, err := url.Parse(c.URL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.Fragment != "" {
		return c, errors.New("tsa: config: url must be an http or https URL without user information")
	}
	if u.RawQuery != "" || u.ForceQuery {
		// the URL goes into the public evidence file: a key in the query
		// would be published
		return c, errors.New("tsa: config: url must not have a query")
	}
	if len(c.URL) > 2048 {
		return c, errors.New("tsa: config: url is too long")
	}
	for i := 0; i < len(c.URL); i++ {
		if c.URL[i] < 0x21 || c.URL[i] > 0x7e {
			return c, errors.New("tsa: config: url must be printable ASCII")
		}
	}
	if c.HashAlgorithm != "" && c.HashAlgorithm != "sha256" {
		return c, errors.New("tsa: config: hashAlgorithm must be sha256")
	}
	if c.PolicyOID != "" {
		if _, err := ParseOID(c.PolicyOID); err != nil {
			return c, errors.New("tsa: config: policyOid is not an OID")
		}
	}
	if c.CABundlePath == "" {
		return c, errors.New("tsa: config: caBundlePath is required")
	}
	if c.SignerKeySHA256 != "" {
		if b, err := hex.DecodeString(c.SignerKeySHA256); err != nil || len(b) != 32 || hex.EncodeToString(b) != c.SignerKeySHA256 {
			return c, errors.New("tsa: config: signerKeySha256 must be 64 lower case hexadecimal digits")
		}
	}
	if c.TimeoutSeconds < 0 || c.TimeoutSeconds > MaxTimeoutSeconds {
		return c, fmt.Errorf("tsa: config: timeoutSeconds must be 0 to %d", MaxTimeoutSeconds)
	}
	if c.MaxSkewSeconds < 0 || c.MaxSkewSeconds > MaxMaxSkewSeconds {
		return c, fmt.Errorf("tsa: config: maxSkewSeconds must be 0 to %d", MaxMaxSkewSeconds)
	}
	return c, nil
}

// ParseBundle splits a PEM bundle. Self-signed certificates (subject equal to
// issuer and a valid self-signature) become trust anchors; every other
// certificate is an intermediate. The bundle must hold at least one
// self-signed certificate and at most MaxBundleCerts certificates; PEM blocks
// other than CERTIFICATE are an error.
func ParseBundle(pemBytes []byte) (roots, intermediates *x509.CertPool, err error) {
	roots, intermediates = x509.NewCertPool(), x509.NewCertPool()
	n, nroots := 0, 0
	rest := pemBytes
	for {
		var b *pem.Block
		b, rest = pem.Decode(rest)
		if b == nil {
			break
		}
		if b.Type != "CERTIFICATE" {
			return nil, nil, errors.New("tsa: bundle holds a PEM block that is not a certificate")
		}
		if n++; n > MaxBundleCerts {
			return nil, nil, errors.New("tsa: bundle holds too many certificates")
		}
		c, err := x509.ParseCertificate(b.Bytes)
		if err != nil {
			return nil, nil, errors.New("tsa: bundle holds a certificate that does not parse")
		}
		if bytes.Equal(c.RawSubject, c.RawIssuer) && c.CheckSignatureFrom(c) == nil {
			roots.AddCert(c)
			nroots++
		} else {
			intermediates.AddCert(c)
		}
	}
	if len(bytes.TrimSpace(rest)) != 0 {
		return nil, nil, errors.New("tsa: bundle has data that is not PEM")
	}
	if nroots == 0 {
		return nil, nil, errors.New("tsa: bundle holds no self-signed trust anchor")
	}
	return roots, intermediates, nil
}

// Options builds a Target.
type Options struct {
	Config Config
	// Roots (required) and Intermediates (optional) come from ParseBundle.
	Roots, Intermediates *x509.CertPool
	// Username and Password, when Username is not empty, are sent as HTTP Basic.
	Username, Password anchor.Secret
	// Client does the HTTP; nil means a client with the default transport.
	// It is copied with its redirect policy replaced (same host only).
	Client *http.Client
	// Clock is the time source (nil: time.Now).
	Clock anchor.Clock
	// Rand is the nonce source (nil: crypto/rand).
	Rand io.Reader
}

// Target is the RFC 3161 target. It implements anchor.Target.
type Target struct {
	cfg    Config
	roots  *x509.CertPool
	inter  *x509.CertPool
	user   func() string // closures: nothing printable holds a credential
	pass   func() string
	clean  func(string, int) string
	client *http.Client
	clock  anchor.Clock
	rand   io.Reader
}

var _ anchor.Target = (*Target)(nil)

// New returns a Target. It reads no file and opens no connection.
func New(o Options) (*Target, error) {
	raw, err := json.Marshal(o.Config)
	if err != nil {
		return nil, err
	}
	cfg, err := ParseConfig(raw)
	if err != nil {
		return nil, err
	}
	if o.Roots == nil {
		return nil, errors.New("tsa: trust anchors are required")
	}
	if o.Username != "" && strings.HasPrefix(cfg.URL, "http://") && !cfg.AllowInsecure {
		return nil, errors.New("tsa: HTTP Basic credentials over http:// need allowInsecure")
	}
	t := &Target{cfg: cfg, roots: o.Roots, inter: o.Intermediates, user: o.Username.Reveal, pass: o.Password.Reveal, clean: anchor.NewRedactor(o.Username, o.Password).Clean,
		client: anchor.Client(o.Client, maxRedirects), clock: o.Clock, rand: o.Rand}
	if t.clock == nil {
		t.clock = time.Now
	}
	if t.rand == nil {
		t.rand = rand.Reader
	}
	return t, nil
}

// Kind implements anchor.Target.
func (t *Target) Kind() string { return anchor.KindTSA }

// Validate implements anchor.Target: the shape of the JSON configuration.
func (t *Target) Validate(cfg json.RawMessage) error {
	_, err := ParseConfig(cfg)
	return err
}

// (the Warning type is gone: the skew bound is applied by Submit itself)

// Upgrade implements anchor.Target: a TSA receipt is final, it is returned unchanged.
func (t *Target) Upgrade(ctx context.Context, r anchor.Receipt) (anchor.Receipt, error) {
	return r, nil
}

// maxSkew is the tolerance between genTime and the clock.
func (t *Target) maxSkew() time.Duration {
	if t.cfg.MaxSkewSeconds > 0 {
		return time.Duration(t.cfg.MaxSkewSeconds) * time.Second
	}
	return DefaultMaxSkew
}

// Submit implements anchor.Target: design 6.4 steps 1 to 5. The token is
// verified before it is accepted, and the time it states is bounded by the
// clock here, in the method of the interface: a genTime further from the
// clock than the tolerance (Config.MaxSkewSeconds, default 5 minutes), in
// either direction, is a failed verification (code "time_skew", class
// refused), and the signing certificate must chain to the roots at the clock
// as well as at genTime ("chain_not_trusted_now").
func (t *Target) Submit(ctx context.Context, p anchor.Payload) (anchor.Receipt, error) {
	if err := anchor.CheckPayload(anchor.KindTSA, p); err != nil {
		return anchor.Receipt{}, err
	}
	info, der, err := t.exchange(ctx, p.Digest)
	if err != nil {
		return anchor.Receipt{}, err
	}
	ev, err := anchor.EncodeEvidence(EvidenceFromToken(t.cfg.URL, info, der))
	if err != nil {
		return anchor.Receipt{}, anchor.Errorf(anchor.ClassPermanent, anchor.KindTSA, "evidence", "evidence", "%s", err.Error())
	}
	gt := info.GenTime.UTC()
	return anchor.Receipt{State: anchor.ReceiptAnchored, Evidence: ev, EvidenceTime: &gt}, nil
}

// Test implements anchor.Target: a real request for the SHA-256 of the empty
// string, with the token verified in full.
func (t *Target) Test(ctx context.Context) anchor.TestResult {
	var res anchor.TestResult
	info, _, err := t.exchange(ctx, sha256.Sum256(nil))
	if err != nil {
		res.Add("time-stamp request", anchor.CheckFailed, "%s", err.Error())
		return res
	}
	res.Add("time-stamp request", anchor.CheckOK, "token granted and verified against the trust anchors")
	res.Add("token", anchor.CheckOK, "genTime %s, policy %s, signer %s", anchor.FormatTime(info.GenTime), info.Policy,
		t.clean(info.Signer.Subject.String(), 200))
	res.Add("clock", anchor.CheckOK, "genTime is within %s of the local clock and the certificate is valid now", t.maxSkew())
	return res
}

// exchange builds the request, posts it, and verifies what comes back.
func (t *Target) exchange(ctx context.Context, digest [32]byte) (*TokenInfo, []byte, error) {
	secs := t.cfg.TimeoutSeconds
	if secs == 0 {
		secs = DefaultTimeoutSeconds
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(secs)*time.Second)
	defer cancel()
	nonce, err := randNonce(t.rand)
	if err != nil {
		return nil, nil, anchor.Errorf(anchor.ClassRetryable, anchor.KindTSA, "request", "nonce", "random source failed")
	}
	body, err := BuildRequest(Request{Digest: digest, PolicyOID: t.cfg.PolicyOID, Nonce: nonce})
	if err != nil {
		return nil, nil, anchor.Errorf(anchor.ClassPermanent, anchor.KindTSA, "request", "build", "%s", err.Error())
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.cfg.URL, bytes.NewReader(body))
	if err != nil {
		return nil, nil, anchor.Errorf(anchor.ClassPermanent, anchor.KindTSA, "request", "build", "bad url")
	}
	req.Header.Set("Content-Type", "application/timestamp-query")
	req.Header.Set("Accept", "application/timestamp-reply")
	if t.user() != "" {
		req.SetBasicAuth(t.user(), t.pass())
	}
	resp, err := t.client.Do(req)
	if err != nil {
		return nil, nil, anchor.TransportError(anchor.KindTSA, "post", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		e := anchor.Errorf(anchor.ClassOfStatus(resp.StatusCode), anchor.KindTSA, "post", "http_status", "unexpected HTTP status")
		e.Status = resp.StatusCode
		e.RetryAfter = anchor.RetryAfter(resp.Header, t.clock())
		return nil, nil, e
	}
	if mt, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type")); err != nil ||
		(mt != "application/timestamp-reply" && mt != "application/timestamp-response") {
		// a captive portal, a proxy or an error page that answers 200 with
		// something else: it may pass, so it is retried (and bounded by the
		// limits of the record)
		return nil, nil, anchor.Errorf(anchor.ClassRetryable, anchor.KindTSA, "post", "content_type", "response is not application/timestamp-reply")
	}
	raw, err := anchor.ReadLimited(resp.Body, MaxResponseBytes)
	if err != nil {
		if errors.Is(err, anchor.ErrTooLarge) {
			return nil, nil, anchor.TransportError(anchor.KindTSA, "post", err)
		}
		return nil, nil, anchor.TransportError(anchor.KindTSA, "read", err)
	}
	pr, err := ParseResponse(raw)
	if err != nil {
		return nil, nil, err
	}
	switch pr.Status {
	case StatusGranted, StatusGrantedWithMod:
		if !pr.HasToken {
			return nil, nil, anchor.Errorf(anchor.ClassPermanent, anchor.KindTSA, "post", "no_token", "status %d without a token", pr.Status)
		}
	case StatusWaiting:
		return nil, nil, anchor.Errorf(anchor.ClassRetryable, anchor.KindTSA, "post", "tsa_waiting", "the TSA answered \"waiting\"%s", t.reason(pr))
	case StatusRejection:
		const systemFailure, timeNotAvailable = 1 << 25, 1 << 14
		class, code := anchor.ClassRefused, "tsa_rejected"
		if pr.FailInfo&(systemFailure|timeNotAvailable) != 0 {
			class, code = anchor.ClassRetryable, "tsa_unavailable"
		}
		return nil, nil, anchor.Errorf(class, anchor.KindTSA, "post", code, "the TSA rejected the request%s", t.reason(pr))
	default:
		return nil, nil, anchor.Errorf(anchor.ClassPermanent, anchor.KindTSA, "post", "tsa_revocation", "the TSA answered with revocation status %d", pr.Status)
	}
	var pin []byte
	if t.cfg.SignerKeySHA256 != "" {
		pin, _ = hex.DecodeString(t.cfg.SignerKeySHA256)
	}
	info, err := VerifyToken(pr.Token, Expect{Digest: digest, Nonce: nonce, Policy: t.cfg.PolicyOID, Now: t.clock(), MaxSkew: t.maxSkew(), SignerKeySHA256: pin}, t.roots, t.inter)
	if err != nil {
		return nil, nil, err
	}
	return info, pr.Token, nil
}

func (t *Target) reason(pr *Response) string {
	s := t.clean(pr.StatusText, 100)
	if s == "" {
		return ""
	}
	return ": " + strings.TrimSpace(s)
}

// String implements fmt.Stringer; it names the target and shows no credential.
func (t *Target) String() string { return "tsa.Target{" + t.cfg.URL + "}" }

// GoString implements fmt.GoStringer.
func (t *Target) GoString() string { return t.String() }

// Format implements fmt.Formatter; every verb prints String(), so no
// credential can be printed through a Target.
func (t *Target) Format(f fmt.State, verb rune) { _, _ = io.WriteString(f, t.String()) }
