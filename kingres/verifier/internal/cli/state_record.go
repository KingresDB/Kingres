// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/KingresDB/Kingres/kingres/verifier/internal/checkpoint"
	"github.com/KingresDB/Kingres/kingres/verifier/internal/format"
	"github.com/KingresDB/Kingres/kingres/verifier/internal/safefile"
	"github.com/KingresDB/Kingres/kingres/verifier/internal/signednote"
)

// The state file is the auditor's record of the last checkpoint this tool
// proved. It holds the signed note itself, so that it is checked again with the
// trust file at every use and so that it is evidence a third party can check,
// and a few facts about when and under which key it was judged.
//
// The invariant of this file: the verifier never writes a state it would not
// read back. Every limit on what is read is a limit on what is written, by the
// same code: the note is bounded where it is parsed (signednote, checkpoint),
// the record built from it cannot exceed maxStateFile whatever the note holds
// (the JSON form of a byte is at most two bytes here), and before a state is
// stored it is read back through decodeHeld and verifyHeld, the functions the
// next run uses, and must give the same checkpoint (see applyState).

// maxStateFile is the largest state file read or written. A note is at most
// signednote.MaxNoteSize bytes and its JSON string form at most twice that (a
// quote, a backslash and a newline take two bytes, a line separator six for
// three); the rest of the record is a few hundred bytes plus the fingerprints
// of at most signednote.MaxSignatures keys.
const maxStateFile = 2*signednote.MaxNoteSize + 16<<10

// maxVerifiedBy is the most signer fingerprints a record holds.
const maxVerifiedBy = signednote.MaxSignatures

type stateRecord struct {
	Format     int      `json:"format"`
	Log        string   `json:"log"`
	Size       uint64   `json:"size"`
	Root       string   `json:"root"`
	VerifiedAt string   `json:"verified_at"`
	VerifiedBy []string `json:"verified_by"`
	Note       string   `json:"note"`
}

// encodeState builds the file for a checkpoint. It refuses a record larger
// than maxStateFile, which cannot happen for a note that passed
// signednote.Parse but is the one place that makes the invariant true.
func encodeState(cp *checkpoint.Checkpoint, note []byte, signers []signednote.Key, now time.Time) ([]byte, error) {
	rec := stateRecord{
		Format: 1, Log: cp.Origin, Size: cp.Size, Root: format.EncodeHex(cp.Root[:]),
		VerifiedAt: now.UTC().Format(time.RFC3339Nano), VerifiedBy: signerPrints(signers), Note: string(note),
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false) // "<" must not grow to six bytes
	enc.SetIndent("", "  ")
	if err := enc.Encode(rec); err != nil {
		return nil, err
	}
	if buf.Len() > maxStateFile {
		return nil, fmt.Errorf("the state record would be %d bytes, more than the %d a later run reads", buf.Len(), maxStateFile)
	}
	return buf.Bytes(), nil
}

// decodeRecord reads a state record strictly: one JSON object, every member
// exactly once and none that is unknown, the right types, nothing after it.
func decodeRecord(data []byte) (*stateRecord, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	tok, err := dec.Token()
	if err != nil {
		return nil, fmt.Errorf("not a state file: %w", err)
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return nil, errors.New("not a state file: expected a JSON object")
	}
	var rec stateRecord
	seen := map[string]bool{}
	for dec.More() {
		kt, err := dec.Token()
		if err != nil {
			return nil, fmt.Errorf("not a state file: %w", err)
		}
		key, _ := kt.(string)
		if seen[key] {
			return nil, fmt.Errorf("not a state file: the member %q appears twice", key)
		}
		seen[key] = true
		var target any
		switch key {
		case "format":
			target = &rec.Format
		case "log":
			target = &rec.Log
		case "size":
			target = &rec.Size
		case "root":
			target = &rec.Root
		case "verified_at":
			target = &rec.VerifiedAt
		case "verified_by":
			target = &rec.VerifiedBy
		case "note":
			target = &rec.Note
		default:
			return nil, fmt.Errorf("not a state file: unknown member %q", key)
		}
		if err := dec.Decode(target); err != nil {
			return nil, fmt.Errorf("not a state file: member %q: %w", key, err)
		}
	}
	if _, err := dec.Token(); err != nil {
		return nil, fmt.Errorf("not a state file: %w", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("not a state file: text after the record")
	}
	for _, m := range []string{"format", "log", "size", "root", "verified_at", "verified_by", "note"} {
		if !seen[m] {
			return nil, fmt.Errorf("not a state file: the member %q is missing", m)
		}
	}
	if rec.Format != 1 {
		return nil, fmt.Errorf("state file format %d is not known, expected 1", rec.Format)
	}
	if len(rec.VerifiedBy) > maxVerifiedBy {
		return nil, fmt.Errorf("not a state file: %d signer fingerprints, expected at most %d", len(rec.VerifiedBy), maxVerifiedBy)
	}
	if _, err := time.Parse(time.RFC3339Nano, rec.VerifiedAt); err != nil {
		return nil, errors.New("not a state file: verified_at is not an RFC 3339 time")
	}
	return &rec, nil
}

// decodeHeld reads a file that holds a checkpoint the auditor kept: a state
// file or a plain signed note. For a state file it returns the record too.
// errEmptyHeld is a held file with no byte in it. It is not the same as no file:
// a state that was there and lost its content (a disk that kept the name and not
// the data after a power loss) must not make the next run record a first
// checkpoint without any check against a rollback.
var errEmptyHeld = errors.New("the file is empty, so it holds no checkpoint (an empty file is not taken for no state at all)")

func decodeHeld(data []byte) ([]byte, *stateRecord, error) {
	if len(data) == 0 {
		return nil, nil, errEmptyHeld
	}
	if !bytes.HasPrefix(data, []byte("{")) {
		if len(data) > signednote.MaxNoteSize {
			return nil, nil, fmt.Errorf("note %s: %d bytes, expected at most %d", safefile.ErrTooLarge, len(data), signednote.MaxNoteSize)
		}
		return data, nil, nil
	}
	if len(data) > maxStateFile {
		return nil, nil, fmt.Errorf("state file %s: %d bytes, expected at most %d", safefile.ErrTooLarge, len(data), maxStateFile)
	}
	rec, err := decodeRecord(data)
	if err != nil {
		return nil, nil, err
	}
	return []byte(rec.Note), rec, nil
}

// agrees checks that the facts of the record are the ones of the note it
// holds. The note decides; a record that says otherwise is damaged or was
// edited, and a reader of the facts must not be misled by it.
func (r *stateRecord) agrees(cp *checkpoint.Checkpoint) error {
	if r.Log != cp.Origin || r.Size != cp.Size || r.Root != format.EncodeHex(cp.Root[:]) {
		return errors.New("the log, size and root of the record are not those of the note it holds")
	}
	return nil
}
