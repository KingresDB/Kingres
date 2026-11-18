// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"encoding/json"
	"time"
)

// The schemas of the signing and settings operations of contract 0.11.0 (ledger phase 7). None of them
// holds a secret: a private key, a seed or a passphrase has no member here.

// LedgerSettingsCluster is the answer of getLedgerSettings and updateLedgerSettings.
type LedgerSettingsCluster struct {
	Publication LedgerPublicationSettings `json:"publication"`
	Anchor      LedgerAnchorSettings      `json:"anchor"`
}

// LedgerPublicationSettings are the publication settings.
type LedgerPublicationSettings struct {
	Enabled            bool    `json:"enabled"`
	OriginPrefix       string  `json:"originPrefix"`
	PublicURL          *string `json:"publicUrl"`
	IntervalSeconds    int     `json:"intervalSeconds"`
	OriginPrefixLocked bool    `json:"originPrefixLocked"`
}

// LedgerAnchorSettings are the anchoring settings.
type LedgerAnchorSettings struct {
	IntervalSeconds int  `json:"intervalSeconds"`
	MinimumAnchored int  `json:"minimumAnchored"`
	Required        bool `json:"required"`
}

// OptionalString is a string member that can be absent (Set false), null (Set true, Value nil) or a value.
type OptionalString struct {
	Set   bool
	Value *string
}

// UnmarshalJSON reads null and a string.
func (o *OptionalString) UnmarshalJSON(b []byte) error {
	o.Set = true
	if string(b) == "null" {
		o.Value = nil
		return nil
	}
	var v string
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	o.Value = &v
	return nil
}

// LedgerSettingsUpdate is the body of updateLedgerSettings; every member is optional. A nil pointer means
// "leave as it is".
type LedgerSettingsUpdate struct {
	Publication *LedgerPublicationUpdate `json:"publication"`
	Anchor      *LedgerAnchorUpdate      `json:"anchor"`
}

// LedgerPublicationUpdate holds the publication members to change.
type LedgerPublicationUpdate struct {
	Enabled         *bool          `json:"enabled"`
	OriginPrefix    *string        `json:"originPrefix"`
	PublicURL       OptionalString `json:"publicUrl"`
	IntervalSeconds *int           `json:"intervalSeconds"`
}

// LedgerAnchorUpdate holds the anchoring members to change.
type LedgerAnchorUpdate struct {
	IntervalSeconds *int  `json:"intervalSeconds"`
	MinimumAnchored *int  `json:"minimumAnchored"`
	Required        *bool `json:"required"`
}

// LedgerSigningKey is one key of LedgerSigning.
type LedgerSigningKey struct {
	Fingerprint      string     `json:"fingerprint"`
	PublicKey        string     `json:"publicKey"`
	State            string     `json:"state"`
	CreatedAt        time.Time  `json:"createdAt"`
	ActivatedAt      *time.Time `json:"activatedAt"`
	RetiredAt        *time.Time `json:"retiredAt"`
	RevokedAt        *time.Time `json:"revokedAt"`
	CompromisedSince *time.Time `json:"compromisedSince"`
	MaxTreeSize      *int64     `json:"maxTreeSize,omitempty"`
}

// LedgerSigningEvent is one event of the key history.
type LedgerSigningEvent struct {
	Seq  int64     `json:"seq"`
	Type string    `json:"type"`
	Time time.Time `json:"time"`
}

// LedgerSigning is the answer of getLedgerSigning.
type LedgerSigning struct {
	ClusterID           *string              `json:"clusterId"`
	Mode                string               `json:"mode"`
	PassphraseProtected bool                 `json:"passphraseProtected"`
	Holders             []string             `json:"holders"`
	Keys                []LedgerSigningKey   `json:"keys"`
	Events              []LedgerSigningEvent `json:"events"`
	BackupAdvised       bool                 `json:"backupAdvised"`
	Discontinuity       bool                 `json:"discontinuity"`
	SignerAlarm         *string              `json:"signerAlarm"`
}

// CreateLedgerSigningKeyRequest is the body of createLedgerSigningKey.
type CreateLedgerSigningKeyRequest struct {
	OverlapDays *int    `json:"overlapDays"`
	PublicKey   *string `json:"publicKey"`
	BackupMade  *bool   `json:"backupMade"`
}

// RevokeLedgerSigningKeyRequest is the body of revokeLedgerSigningKey.
type RevokeLedgerSigningKeyRequest struct {
	CompromisedSince time.Time `json:"compromisedSince"`
	Reason           string    `json:"reason"`
	Confirm          string    `json:"confirm"`
}
