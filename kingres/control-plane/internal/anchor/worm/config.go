// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package worm

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// Limits and defaults.
const (
	DefaultKeyPrefix     = "kingres-anchors"
	DefaultRetentionDays = 3650
	MinRetentionDays     = 1
	MaxRetentionDays     = 36500
	MaxConfigBytes       = 16 << 10
)

// Modes of Object Lock.
const (
	ModeGovernance = "governance"
	ModeCompliance = "compliance"
)

// IfNoneMatch settings.
const (
	IfNoneMatchAuto = "auto"
	IfNoneMatchOn   = "on"
	IfNoneMatchOff  = "off"
)

// Config is the non-secret configuration of a WORM target (design 6.3).
// JSON member names are the lower camel case of the design.
type Config struct {
	// Endpoint is the store's base URL ("https://host[:port]"); empty for AWS.
	Endpoint string `json:"endpoint"`
	Region   string `json:"region"`
	Bucket   string `json:"bucket"`
	// KeyPrefix defaults to DefaultKeyPrefix.
	KeyPrefix string `json:"keyPrefix"`
	// PathStyle defaults to true when Endpoint is set, else false.
	PathStyle *bool `json:"pathStyle"`
	// Mode is "compliance" (the default: nobody, the account root included,
	// can delete or shorten the retention before it ends) or "governance" (an
	// explicit choice that Warnings and Test report: a principal with
	// s3:BypassGovernanceRetention can delete the version).
	Mode string `json:"mode"`
	// RetentionDays defaults to 3650, range 1 to 36500.
	RetentionDays int `json:"retentionDays"`
	// IfNoneMatch is "auto" (default), "on" or "off".
	IfNoneMatch string `json:"ifNoneMatch"`
	// CABundlePath is kept for the caller, which builds the *http.Client
	// from it; this package reads no file.
	CABundlePath string `json:"caBundlePath"`
	// AllowInsecure permits an http:// endpoint (a test store on a private
	// network). Without it every URL must be https.
	AllowInsecure bool `json:"allowInsecure"`
	// MaxSkewSeconds is the tolerance of the store's time: a fresh write must
	// have a Last-Modified within it of the clock of Submit both ways, and an
	// existing object one not earlier than the start of the first Submit call
	// less it (see the package documentation; 0 means 300, the range is 1 to
	// 86400). A store whose time is further off is refused (code
	// "store_time_skew" or "store_time_early").
	MaxSkewSeconds int `json:"maxSkewSeconds"`
}

// MaxMaxSkewSeconds is the largest MaxSkewSeconds.
const MaxMaxSkewSeconds = 86400

// Warnings lists what an operator must be told about the configuration: the
// governance mode, in which a principal with s3:BypassGovernanceRetention can
// still delete an anchored object (so an anchor is not the same assurance as
// in compliance mode).
func (c Config) Warnings() []string {
	if c.Mode == ModeGovernance {
		return []string{"governance mode: a principal with s3:BypassGovernanceRetention can delete or shorten the retention of an anchored object; only compliance mode cannot be bypassed"}
	}
	return nil
}

// ParseConfig parses and validates the JSON configuration, rejecting
// unknown members, and returns it with the defaults filled in.
func ParseConfig(raw json.RawMessage) (Config, error) {
	var c Config
	if len(raw) > MaxConfigBytes {
		return c, errors.New("worm: configuration is too large")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return c, fmt.Errorf("worm: configuration: %w", err)
	}
	return c.Normalize()
}

func okKeyChars(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.' || c == '/') {
			return false
		}
	}
	return true
}

// Normalize validates c and fills the defaults.
func (c Config) Normalize() (Config, error) {
	if c.Region == "" || len(c.Region) > 64 || !okKeyChars(c.Region) || strings.Contains(c.Region, "/") {
		return c, errors.New("worm: region is required (letters, digits, - _ .)")
	}
	if n := len(c.Bucket); n < 3 || n > 63 || !okKeyChars(c.Bucket) || strings.Contains(c.Bucket, "/") ||
		strings.HasPrefix(c.Bucket, ".") || strings.HasSuffix(c.Bucket, ".") || strings.Contains(c.Bucket, "..") {
		return c, errors.New("worm: bucket name is not valid")
	}
	if c.KeyPrefix == "" {
		c.KeyPrefix = DefaultKeyPrefix
	}
	c.KeyPrefix = strings.Trim(c.KeyPrefix, "/")
	if c.KeyPrefix == "" || len(c.KeyPrefix) > 200 || !okKeyChars(c.KeyPrefix) {
		return c, errors.New("worm: keyPrefix must be 1 to 200 characters of letters, digits, - _ . /")
	}
	for _, seg := range strings.Split(c.KeyPrefix, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return c, errors.New("worm: keyPrefix has an empty or dot segment")
		}
	}
	switch c.Mode {
	case "":
		c.Mode = ModeCompliance
	case ModeGovernance, ModeCompliance:
	default:
		return c, errors.New("worm: mode must be governance or compliance")
	}
	if c.RetentionDays == 0 {
		c.RetentionDays = DefaultRetentionDays
	}
	if c.RetentionDays < MinRetentionDays || c.RetentionDays > MaxRetentionDays {
		return c, fmt.Errorf("worm: retentionDays must be %d to %d", MinRetentionDays, MaxRetentionDays)
	}
	switch c.IfNoneMatch {
	case "":
		c.IfNoneMatch = IfNoneMatchAuto
	case IfNoneMatchAuto, IfNoneMatchOn, IfNoneMatchOff:
	default:
		return c, errors.New("worm: ifNoneMatch must be auto, on or off")
	}
	if c.MaxSkewSeconds < 0 || c.MaxSkewSeconds > MaxMaxSkewSeconds {
		return c, fmt.Errorf("worm: maxSkewSeconds must be 0 to %d", MaxMaxSkewSeconds)
	}
	if len(c.CABundlePath) > 1024 {
		return c, errors.New("worm: caBundlePath is too long")
	}
	if c.Endpoint != "" {
		u, err := url.Parse(c.Endpoint)
		if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" ||
			(u.Path != "" && u.Path != "/") {
			return c, errors.New("worm: endpoint must be scheme://host[:port] without path, user information or query")
		}
		switch u.Scheme {
		case "https":
		case "http":
			if !c.AllowInsecure {
				return c, errors.New("worm: endpoint is not https and allowInsecure is not set")
			}
		default:
			return c, errors.New("worm: endpoint scheme must be https")
		}
		c.Endpoint = u.Scheme + "://" + strings.ToLower(u.Host)
		if c.PathStyle == nil {
			t := true
			c.PathStyle = &t
		}
	} else if c.PathStyle == nil {
		f := false
		c.PathStyle = &f
	}
	return c, nil
}

// lockMode returns the header value of the mode.
func (c Config) lockMode() string { return strings.ToUpper(c.Mode) }

// ObjectKey is the key of the note: "<prefix>/<aggregate id hex>/<size as 20
// decimal digits, zero padded>.note" (design 6.3 step 1).
func ObjectKey(prefix, aggregateIDHex string, size uint64) string {
	return ObjectKeyGen(prefix, aggregateIDHex, size, 0)
}

// ObjectKeyGen is the key of generation gen of the note (correction 95):
// generation 0 is exactly ObjectKey, and generation n above 0 is
// "<prefix>/<aggregate id hex>/<size as 20 decimal digits>.g<n>.note".
func ObjectKeyGen(prefix, aggregateIDHex string, size uint64, gen uint32) string {
	s := strconv.FormatUint(size, 10)
	g := ""
	if gen > 0 {
		g = ".g" + strconv.FormatUint(uint64(gen), 10)
	}
	return strings.Trim(prefix, "/") + "/" + aggregateIDHex + "/" + strings.Repeat("0", 20-len(s)) + s + g + ".note"
}
