// SPDX-License-Identifier: AGPL-3.0-only

package cli

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/KingresDB/Kingres/kingres/verifier/internal/trust"
)

// runTrustFollow is `trust follow`: it reads the events of a publication, verifies each key-added,
// key-retired and key-revoked event under the keys already trusted (a key an event added counts
// from that event on), and writes a new trust file. It never writes the file it read (the old one
// stays), and it never starts trust: the first key is the one the auditor pinned (3.4).
func (e *env) runTrustFollow(args []string) int {
	fs, asJSON := e.newFlagsWithExit("trust follow", "--trust FILE --source SOURCE --out FILE [--now TIME]",
		"Follow the rotation of the signing keys: read events/000000, events/000001 ... of the publication,\n"+
			"verify each one under the keys the trust file pins (or that a verified earlier event added), and\n"+
			"write a new trust file with the keys added, the keys retired (limited to the size the event\n"+
			"gives, or unchanged when the event says unbounded) and the keys revoked. A key-added event must\n"+
			"be signed by a trusted key and by the new key, which has to pass the three tests of the\n"+
			"extension (canonical encoding, on the curve, not of small order); a key-retired and a\n"+
			"key-revoked event by a trusted key other than the one it is about. An event that is signed by no\n"+
			"such key stops the run (status 1). --out must not exist and is never the file read. Start from the\n"+
			"file you pinned yourself: a key the file calls revoked does not vouch for any event. The\n"+
			"publication is never a source of first trust.", "0 written, 1 an event failed to verify, 2 usage or input error,\n3 the publication could not be read, or holds no event at all (not even the first key's).")
	trustPath := fs.String("trust", "", "the trust file you pinned (JSON)")
	source := fs.String("source", ".", "directory, .tar.gz or https address of the publication")
	out := fs.String("out", "", "the new trust file to write (must not exist)")
	nowFlag := fs.String("now", "", "time used for the notes about validity windows, RFC 3339 (default: system clock)")
	if code, done := e.parseFlags(fs, args); done {
		return code
	}
	const cmd = "trust follow"
	r := &result{cmd: cmd}
	o := e.trustFollowRun(r, *trustPath, *source, *out, nowOf(fs, nowFlag))
	switch o.kind {
	case outInput:
		return e.inputErrorInfo(cmd, *asJSON, o.err, r.inputInfo())
	case outFailed:
		return e.finishFailed(r, *asJSON, o.step, o.err)
	case outIncomplete:
		return e.finishIncomplete(r, *asJSON, o.missing)
	}
	return e.finishOK(r, *asJSON)
}

func (e *env) trustFollowRun(r *result, trustPath, source, out string, now nowArg) outcome {
	if out == "" {
		return inputOut(errors.New("missing --out: the new trust file is written beside the old one, never over it"))
	}
	p, err := loadPubInputsOpt(trustPath, source, now, false)
	if err != nil {
		return inputOut(err)
	}
	defer p.close()
	r.notes = append(r.notes, p.notes...)
	if same, _ := sameFile(trustPath, out); same {
		return inputOut(errors.New("--out is the trust file that was read: the new file is written beside the old one, never over it"))
	}
	if _, err := os.Lstat(out); err == nil {
		return inputOut(fmt.Errorf("--out %s exists: it is not overwritten", out))
	}
	w, err := readEvents(p.base, p.src, true)
	if err != nil {
		return eventsOutcome(err, "the events of the publication could not be read")
	}
	if w.Count == 0 {
		return incompleteOut("the publication holds no event: not even the event of the first key (events/000000). It may have been withheld; nothing was followed and nothing was written")
	}
	nf := &trust.File{OriginPrefix: p.base.OriginPrefix, TSARoots: p.base.TSARoots, ClientKeys: p.base.ClientKeys}
	byFP := map[string]trust.Key{}
	for _, k := range p.base.Keys {
		byFP[k.Fingerprint] = k
	}
	names := map[string]bool{}
	for _, k := range p.base.Keys {
		names[k.Name] = true
	}
	changes := 0
	for _, k := range w.Keys {
		tk, existed := byFP[k.Fingerprint]
		if !existed {
			name := k.Name
			for i := 2; names[name]; i++ {
				name = fmt.Sprintf("%s-%d", k.Name, i)
			}
			names[name] = true
			tk = trust.Key{Name: name, Public: k.Public, Fingerprint: k.Fingerprint, NotBefore: k.NotBefore}
			r.ok("key-added", fmt.Sprintf("event %06d adds key %s, valid from %s", k.AddedBy, k.Fingerprint[:16], k.NotBefore.UTC().Format(time.RFC3339)))
			changes++
		}
		if k.RetiredByEvent && (tk.MaxTreeSize == nil || (k.MaxTreeSize != nil && *k.MaxTreeSize < *tk.MaxTreeSize)) && !k.Revoked {
			tk.MaxTreeSize = k.MaxTreeSize
			if k.MaxTreeSize != nil {
				r.ok("key-retired", fmt.Sprintf("event %06d retires key %s: it counts for checkpoints of tree size %d or less", k.RetiredBy, k.Fingerprint[:16], *k.MaxTreeSize))
				changes++
			} else {
				r.ok("key-retired", fmt.Sprintf("event %06d retires key %s with no bound: the trust file keeps it usable for any size", k.RetiredBy, k.Fingerprint[:16]))
			}
		}
		if k.Revoked && !k.RevokedInFile {
			tk.Revoked, tk.MaxTreeSize = true, nil
			r.ok("key-revoked", fmt.Sprintf("event %06d revokes key %s (compromised since %s: %s)", k.RevokedBy, k.Fingerprint[:16], k.CompromisedSince.UTC().Format(time.RFC3339), k.RevokeReason))
			changes++
		}
		nf.Keys = append(nf.Keys, tk)
	}
	for _, n := range w.Notes {
		r.notes = append(r.notes, n)
	}
	for t, n := range w.Other {
		r.notes = append(r.notes, fmt.Sprintf("%d event(s) of type %s were verified and are not interpreted by this version", n, t))
	}
	data, err := nf.Marshal()
	if err != nil {
		return inputOut(err)
	}
	if back, err := trust.Parse(data); err != nil || len(back.Keys) != len(nf.Keys) {
		return inputOut(fmt.Errorf("the new trust file would not read back (%v): nothing was written", err))
	}
	if err := writeNewFile(out, data); err != nil {
		return inputOut(fmt.Errorf("not writing %s: %w", out, err))
	}
	r.ok("events", fmt.Sprintf("%d event(s) read and verified; %d change(s) to the trust file", w.Count, changes))
	r.notes = append(r.notes, "the new trust file is "+out+"; the file you read was not changed. Keep it where the cluster's administrators cannot change it",
		"validity windows were not applied to the events: an event is a statement about the past")
	r.notProved = []string{
		"that the publication shows every event: a publisher that stops serving events at some number looks like one that has no more; compare the count with the one of your last run",
		"that a revoked key's signatures made before the revocation are good: the trust file marks the key revoked, and its past checkpoints are suspect (the anchors that would date them are a later step)",
	}
	r.headline = fmt.Sprintf("%d event(s) of the publication verified under the trust file; the new trust file %s has %d key(s).", w.Count, out, len(nf.Keys))
	return outcome{}
}

// sameFile reports whether two paths name the same file (when both exist), or the same name.
func sameFile(a, b string) (bool, error) {
	if filepath.Clean(a) == filepath.Clean(b) {
		return true, nil
	}
	sa, err := os.Stat(a)
	if err != nil {
		return false, err
	}
	sb, err := os.Stat(b)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	return os.SameFile(sa, sb), nil
}

// writeNewFile writes data to a file that must not exist (O_EXCL). A write that fails removes the
// file it made, so that an empty file does not make the next run refuse the name.
func writeNewFile(path string, data []byte) error {
	fh, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644) // #nosec G304 -- the path is a command line argument
	if err != nil {
		return err
	}
	if _, err := trustWrite(fh, data); err != nil {
		_ = fh.Close()
		_ = os.Remove(path)
		return err
	}
	if err := fh.Close(); err != nil {
		_ = os.Remove(path)
		return err
	}
	return nil
}
