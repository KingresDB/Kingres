// SPDX-License-Identifier: AGPL-3.0-only

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
	// Mode is "governance" (default) or "compliance".
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
		c.Mode = ModeGovernance
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
	s := strconv.FormatUint(size, 10)
	return strings.Trim(prefix, "/") + "/" + aggregateIDHex + "/" + strings.Repeat("0", 20-len(s)) + s + ".note"
}
