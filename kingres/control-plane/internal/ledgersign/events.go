// SPDX-License-Identifier: AGPL-3.0-only

package ledgersign

import (
	"bytes"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// The events of the key history (design 3.2.4). An event is a signed note whose key name is the origin
// prefix of the cluster; the text is ASCII:
//
//	kingres-ledger-event/1
//	seq: <decimal>
//	time: <RFC 3339 UTC, whole seconds>
//	type: <key-added | key-retired | key-revoked | ...>
//	<type-specific "name: value" lines>
//
// Events are numbered from 0 without gaps. The lines of the three types of this step:
//
//	key-added:   key (base64 public key), fingerprint (hex), not-before (the activation time)
//	key-retired: key, max-tree-size (a decimal or "unbounded")
//	key-revoked: key, compromised-since, reason (at most 200 bytes of printable ASCII)
const (
	EventKeyAdded   = "key-added"
	EventKeyRetired = "key-retired"
	EventKeyRevoked = "key-revoked"

	eventHeader = "kingres-ledger-event/1"
	maxFreeText = 200
)

type field struct{ name, value string }

// eventText builds the text of an event. Every value is checked to be printable ASCII.
func eventText(seq int64, at time.Time, typ string, fields ...field) ([]byte, error) {
	if seq < 0 {
		return nil, errors.New("ledgersign: an event number is not negative")
	}
	var b bytes.Buffer
	b.WriteString(eventHeader + "\n")
	b.WriteString("seq: " + strconv.FormatInt(seq, 10) + "\n")
	b.WriteString("time: " + at.UTC().Truncate(time.Second).Format(time.RFC3339) + "\n")
	b.WriteString("type: " + typ + "\n")
	for _, f := range fields {
		if !printableASCII(f.name) || !printableASCII(f.value) || strings.ContainsAny(f.name, ": ") {
			return nil, fmt.Errorf("ledgersign: the line %q of an event is not printable ASCII", clipText(f.name))
		}
		if len(f.value) > maxFreeText && f.name == "reason" {
			return nil, errors.New("ledgersign: the reason of an event is at most 200 bytes")
		}
		b.WriteString(f.name + ": " + f.value + "\n")
	}
	return b.Bytes(), nil
}

// printableASCII is true for text of the bytes 0x20 to 0x7e only.
func printableASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] > 0x7e {
			return false
		}
	}
	return true
}

// ValidReason checks the free text of a revocation: at most 200 bytes of printable ASCII.
func ValidReason(s string) error {
	switch {
	case len(s) > maxFreeText:
		return fmt.Errorf("must be at most %d bytes", maxFreeText)
	case !printableASCII(s):
		return errors.New("must be printable ASCII")
	}
	return nil
}

// eventFields reads the "name: value" lines of the text of an event (after the four fixed ones).
func eventFields(text []byte) (map[string]string, error) {
	lines := strings.Split(strings.TrimSuffix(string(text), "\n"), "\n")
	if len(lines) < 4 || lines[0] != eventHeader {
		return nil, errors.New("ledgersign: not an event")
	}
	out := map[string]string{}
	for _, l := range lines[1:] {
		name, value, ok := strings.Cut(l, ": ")
		if !ok {
			return nil, errors.New("ledgersign: a line of an event has no value")
		}
		out[name] = value
	}
	return out, nil
}

// noteText is the text of a stored signed note: everything before the blank line.
func noteText(note []byte) []byte {
	i := bytes.Index(note, []byte("\n\n"))
	if i < 0 {
		return nil
	}
	return note[:i+1]
}
