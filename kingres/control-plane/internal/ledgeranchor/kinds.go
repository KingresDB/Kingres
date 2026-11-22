// SPDX-License-Identifier: AGPL-3.0-only

package ledgeranchor

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/anchor"
)

// Info is what a kind says about a configuration it has normalised: where the requests of the target may go, the
// CA bundle and what the credentials are.
type Info struct {
	// URLs are the URLs the target may reach (Egress).
	URLs []string
	// ExtraHosts are further "host:port" values allowed (the host of a virtual-hosted bucket).
	ExtraHosts []string
	// CABundle is the path of a PEM bundle ("" for none); CARequired says the kind needs one.
	CABundle string
	// TLSSystemRoots says that the connection is checked against the roots of the system and the CA bundle is for something else (the token).
	TLSSystemRoots bool
	// Warnings are texts the operator is told with the result of a create or a test (not errors).
	Warnings []string
}

// BuildEnv is what a kind needs to make its target.
type BuildEnv struct {
	// Config is the normalised configuration the kind returned (without credentialsPath).
	Config json.RawMessage
	Info   Info
	Creds  Creds
	Egress *Egress
	// CAPEM is the content of the CA bundle ("" when there is none).
	CAPEM []byte
	// Client is made from the egress rule and the CA bundle of the configuration.
	Client *http.Client
	Clock  anchor.Clock
	// TargetID is the id of the target (some kinds name it in what they store).
	TargetID int64
}

// Kind is the wiring of one kind of target.
type Kind struct {
	Name string
	// Normalize validates the configuration (without credentialsPath) and returns it with the defaults filled in.
	Normalize func(cfg json.RawMessage) (json.RawMessage, Info, error)
	// CredentialsRequired says that a target of the kind cannot be built without credentials; CredentialsAllowed
	// that it may have some.
	CredentialsRequired bool
	CredentialsAllowed  bool
	// CARequired says that the kind needs a CA bundle in its configuration (the token of an authority is checked
	// against it); a kind that does not takes the roots of the system when there is none.
	CARequired bool
	// CheckBundle, when set, checks the content of the CA bundle when the target is made.
	CheckBundle func(pem []byte) error
	// Build makes the target.
	Build func(env BuildEnv) (anchor.Target, error)
	// Timeouts returns how long a submitted and a claimed record may wait before it expires (zero: never) for a
	// configuration; only OpenTimestamps waits.
	Timeouts func(cfg json.RawMessage) (submitted, claimed time.Duration)
	// PriorEvidence returns the evidence that a failed call left in its error (OpenTimestamps: what the calendars that answered gave), which the record must keep (duty 12).
	PriorEvidence func(err error) (json.RawMessage, bool)
	// Binds checks that the evidence a call returned (canonical, valid for the kind) is the evidence of this record: what the evidence
	// states of the checkpoint (the key and the digest of the stored note, the digest, the imprint of the token) is what the row
	// holds. It is the second check behind the library's own, at the one place where a record becomes anchored (P7A-05); an error fails
	// the record for good.
	Binds func(r Row, evidence json.RawMessage) error
	// Exports are the file formats besides JSON that downloadLedgerAnchor can write for the kind ("ots", "tsr").
	Exports map[string]func(Row) ([]byte, error)
}

var kinds = map[string]*Kind{}

// register adds a kind; the files of the kinds call it from init, so a build that leaves a file out has no such kind.
func register(k *Kind) { kinds[k.Name] = k }

// KindNames lists the registered kinds.
func KindNames() []string {
	var out []string
	for n := range kinds {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// credentialsPathKey is the member of a configuration that names the operator's own credentials file; it is of
// no kind's library, so it is taken out before the library reads the configuration.
const credentialsPathKey = "credentialsPath"

// splitConfig separates credentialsPath from a configuration. A configuration is a JSON object of at most 16 KiB.
func splitConfig(raw json.RawMessage) (rest json.RawMessage, credPath string, err error) {
	if len(raw) > 16<<10 {
		return nil, "", errors.New("the configuration is too large")
	}
	var m map[string]json.RawMessage
	dec := json.NewDecoder(bytes.NewReader(raw))
	if err := dec.Decode(&m); err != nil || m == nil || dec.More() {
		return nil, "", errors.New("the configuration must be a JSON object")
	}
	if v, ok := m[credentialsPathKey]; ok {
		var p string
		if err := json.Unmarshal(v, &p); err != nil || p == "" || len(p) > 1024 {
			return nil, "", errors.New("credentialsPath must be a path of at most 1024 characters")
		}
		credPath = p
		delete(m, credentialsPathKey)
	}
	b, err := json.Marshal(m)
	if err != nil {
		return nil, "", errors.New("the configuration is not valid")
	}
	return b, credPath, nil
}

// joinConfig adds credentialsPath back to a normalised configuration.
func joinConfig(cfg json.RawMessage, credPath string) (json.RawMessage, error) {
	if credPath == "" {
		return cfg, nil
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(cfg, &m); err != nil {
		return nil, err
	}
	p, _ := json.Marshal(credPath)
	m[credentialsPathKey] = p
	return json.Marshal(m)
}

// kindOf returns the registered kind.
func kindOf(name string) (*Kind, error) {
	k, ok := kinds[name]
	if !ok {
		return nil, fmt.Errorf("the kind %q is not available on this server", clipName(name))
	}
	return k, nil
}

func clipName(s string) string {
	if len(s) > 20 {
		return s[:20] + "..."
	}
	return s
}
