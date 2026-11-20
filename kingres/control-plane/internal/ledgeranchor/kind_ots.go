// SPDX-License-Identifier: AGPL-3.0-only

package ledgeranchor

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/anchor"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/anchor/ots"
)

// The OpenTimestamps kind (design 6.5, step 14). The configuration is the library's (calendars, minCalendars, upgradeTimeoutDays,
// allowInsecure, contradictionConfirmMinutes) and one member of this package, headerSource (correction 185):
//
//	"headerSource": {"url": "https://node.example/api"}
//
// the base URL of an Esplora-compatible block explorer of a node the operator trusts (GET <url>/block-height/<h> answers the hash of the block,
// GET <url>/block/<hash>/header the 80-byte header in hexadecimal). Without it a claim of a calendar stays a claim (state claimed, shown as
// submitted, never counted as anchored); with it a record is anchored only when the header of the claimed height confirms the claim (the
// library's rules: merkle root, proof of work, time). The calendars and the source are the hosts the egress rule allows; nothing else is reached.
func init() {
	register(&Kind{
		Name:               anchor.KindOTS,
		CredentialsAllowed: false,
		Normalize:          normalizeOTS,
		Build:              buildOTS,
		Binds:              bindsOTS,
		Timeouts: func(cfg json.RawMessage) (time.Duration, time.Duration) {
			c, _, err := splitOTS(cfg)
			if err != nil {
				return 0, 0
			}
			return ots.Timeout(c.UpgradeTimeoutDays), ots.ClaimedTimeout(c.UpgradeTimeoutDays)
		},
		PriorEvidence: ots.PriorEvidenceRaw,
		Exports:       map[string]func(Row) ([]byte, error){"ots": exportOTS},
	})
}

// bindsOTS: the digest that the calendars stamped is the digest of the record.
func bindsOTS(r Row, raw json.RawMessage) error {
	ev, err := anchor.ParseOTSEvidence(raw)
	if err != nil {
		return err
	}
	if ev.Digest != hex.EncodeToString(r.PayloadSHA256[:]) {
		return errors.New("the digest of the evidence is not the digest of this checkpoint")
	}
	return nil
}

type headerSourceCfg struct {
	URL string `json:"url"`
}

// splitOTS separates headerSource from the library's configuration.
func splitOTS(raw json.RawMessage) (ots.Config, *headerSourceCfg, error) {
	var m map[string]json.RawMessage
	dec := json.NewDecoder(bytes.NewReader(raw))
	if err := dec.Decode(&m); err != nil || m == nil {
		return ots.Config{}, nil, errors.New("the configuration must be a JSON object")
	}
	var hs *headerSourceCfg
	if v, ok := m["headerSource"]; ok {
		hs = &headerSourceCfg{}
		d := json.NewDecoder(bytes.NewReader(v))
		d.DisallowUnknownFields()
		if err := d.Decode(hs); err != nil {
			return ots.Config{}, nil, errors.New("headerSource must be {\"url\": \"...\"}")
		}
		delete(m, "headerSource")
	}
	rest, _ := json.Marshal(m)
	c, err := ots.ParseConfig(rest)
	return c, hs, err
}

func normalizeOTS(raw json.RawMessage) (json.RawMessage, Info, error) {
	c, hs, err := splitOTS(raw)
	if err != nil {
		return nil, Info{}, err
	}
	info := Info{URLs: append([]string(nil), c.Calendars...)}
	out := map[string]any{}
	b, _ := json.Marshal(c)
	_ = json.Unmarshal(b, &out)
	if hs != nil {
		u, err := url.Parse(hs.URL)
		if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Scheme != "https" && !(u.Scheme == "http" && c.AllowInsecure)) {
			return nil, Info{}, errors.New("headerSource.url must be an https URL (http only with allowInsecure) without user information or query")
		}
		hs.URL = strings.TrimRight(hs.URL, "/")
		out["headerSource"] = hs
		info.URLs = append(info.URLs, hs.URL)
	}
	res, err := json.Marshal(out)
	if err != nil {
		return nil, Info{}, errors.New("the configuration is not valid")
	}
	return res, info, nil
}

func buildOTS(env BuildEnv) (anchor.Target, error) {
	c, hs, err := splitOTS(env.Config)
	if err != nil {
		return nil, err
	}
	var src ots.HeaderSource
	if hs != nil {
		src = &esplora{base: hs.URL, hc: env.Client}
	}
	t, err := ots.New(c, ots.Options{Client: env.Client, Clock: env.Clock, UserAgent: "kingresd", Headers: src})
	if err != nil {
		return nil, err
	}
	return &otsTarget{Target: t, now: env.Clock}, nil
}

// otsTarget adds the warning about the age of the table of real headers to the Test of the library (duty 14); Submit, Upgrade and Resume are the
// library's.
type otsTarget struct {
	*ots.Target
	now anchor.Clock
}

func (t *otsTarget) Test(ctx context.Context) anchor.TestResult {
	res := t.Target.Test(ctx)
	if age := ots.NewestCheckpointAge(t.now()); age > ots.CheckpointWarnAge {
		res.Add("header-table", anchor.CheckWarning, "the table of real block headers of this build is %d days old (the newest entry is older than a year): the work rule of a claim is weaker than it should be; use a newer release", int(age.Hours()/24))
	}
	return res
}

func exportOTS(r Row) ([]byte, error) {
	ev, err := anchor.ParseOTSEvidence(r.Evidence)
	if err != nil {
		return nil, err
	}
	return ots.Export(ev)
}

// warnHeaderTable logs once, at the start, that the table of real headers is older than a year (duty 14).
func warnHeaderTable(log *slog.Logger, now time.Time) {
	if age := ots.NewestCheckpointAge(now); age > ots.CheckpointWarnAge {
		log.Warn("the table of real block headers of OpenTimestamps in this build is older than a year: extend it at the next release", "days", int(age.Hours()/24))
	}
}

// esplora is a HeaderSource over an Esplora-compatible explorer: bounded answers, the hash checked against the header.
type esplora struct {
	base string
	hc   *http.Client
}

func (e *esplora) get(ctx context.Context, path string, max int64) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, e.base+path, nil)
	if err != nil {
		return "", errors.New("the header source request could not be built")
	}
	resp, err := anchor.Client(e.hc, 0).Do(req)
	if err != nil {
		return "", anchor.TransportError("ots", "headers", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := anchor.ReadLimited(resp.Body, max)
	if err != nil {
		return "", errors.New("the header source answered more than the limit")
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("the header source answered http %d", resp.StatusCode)
	}
	return strings.TrimSpace(string(body)), nil
}

// Header implements ots.HeaderSource.
func (e *esplora) Header(ctx context.Context, height uint64) ([]byte, error) {
	hash, err := e.get(ctx, "/block-height/"+strconv.FormatUint(height, 10), 128)
	if err != nil {
		return nil, err
	}
	if b, err := hex.DecodeString(hash); err != nil || len(b) != 32 {
		return nil, errors.New("the header source did not answer a block hash")
	}
	hx, err := e.get(ctx, "/block/"+hash+"/header", 400)
	if err != nil {
		return nil, err
	}
	h, err := hex.DecodeString(hx)
	if err != nil || len(h) != 80 {
		return nil, errors.New("the header source did not answer an 80-byte header")
	}
	// the hash that was named must be the hash of the header that was given
	d := sha256.Sum256(h)
	d = sha256.Sum256(d[:])
	for i, j := 0, len(d)-1; i < j; i, j = i+1, j-1 {
		d[i], d[j] = d[j], d[i]
	}
	if hex.EncodeToString(d[:]) != strings.ToLower(hash) {
		return nil, errors.New("the header does not hash to the block that was named")
	}
	return h, nil
}
