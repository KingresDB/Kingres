// SPDX-License-Identifier: AGPL-3.0-only

package ledgeranchor

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/anchor"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/anchor/tsa"
)

// The RFC 3161 kind (design 6.4, step 12). The configuration is the library's (url, caBundlePath, policyOid, timeoutSeconds, maxSkewSeconds,
// signerKeySha256, hashAlgorithm, allowInsecure); caBundlePath holds the trust anchors and intermediates that the TOKEN is checked against
// (it is required). The TLS connection to the authority is checked against the roots of the system, not against that bundle (an authority
// is usually on a public CA, and its token chain is a different matter). The credentials are an optional HTTP Basic pair. The request goes
// to the host and port of the url and to no other.
func init() {
	register(&Kind{
		Name:                anchor.KindTSA,
		CredentialsAllowed:  true,
		CredentialsRequired: false,
		CARequired:          true,
		Normalize:           normalizeTSA,
		CheckBundle:         func(pem []byte) error { _, _, err := tsa.ParseBundle(pem); return err },
		Build:               buildTSA,
		Timeouts:            func(json.RawMessage) (time.Duration, time.Duration) { return 0, 0 },
		Exports:             map[string]func(Row) ([]byte, error){"tsr": exportTSR},
	})
}

func normalizeTSA(raw json.RawMessage) (json.RawMessage, Info, error) {
	c, err := tsa.ParseConfig(raw)
	if err != nil {
		return nil, Info{}, err
	}
	out, err := json.Marshal(c)
	if err != nil {
		return nil, Info{}, errors.New("the configuration is not valid")
	}
	return out, Info{URLs: []string{c.URL}, CABundle: c.CABundlePath, TLSSystemRoots: true}, nil
}

func buildTSA(env BuildEnv) (anchor.Target, error) {
	var c tsa.Config
	if err := json.Unmarshal(env.Config, &c); err != nil {
		return nil, errors.New("the configuration is not valid")
	}
	roots, inter, err := tsa.ParseBundle(env.CAPEM)
	if err != nil {
		return nil, err
	}
	return tsa.New(tsa.Options{Config: c, Roots: roots, Intermediates: inter,
		Username: env.Creds.get("username"), Password: env.Creds.get("password"), Client: env.Client, Clock: env.Clock})
}

// exportTSR writes the time-stamp token of the evidence as the DER file of the format "tsr": the CMS ContentInfo (the TimeStampToken) that
// openssl ts -verify -token_in reads (correction 184: it is the token, not the whole TimeStampResp, which the cluster does not keep).
func exportTSR(r Row) ([]byte, error) {
	ev, err := anchor.ParseTSAEvidence(r.Evidence)
	if err != nil {
		return nil, err
	}
	return base64.StdEncoding.DecodeString(ev.TokenDERB64)
}
