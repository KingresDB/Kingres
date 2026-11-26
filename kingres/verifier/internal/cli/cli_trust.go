// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package cli

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/KingresDB/Kingres/kingres/verifier/internal/signednote"
	"github.com/KingresDB/Kingres/kingres/verifier/internal/trust"
)

func (e *env) runTrust(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(e.stderr, "kingres-verify trust: expected init, show or follow")
		return exit(ExitUsage)
	}
	switch args[0] {
	case "init":
		return e.runTrustInit(args[1:])
	case "show":
		return e.runTrustShow(args[1:])
	case "follow":
		return e.runTrustFollow(args[1:])
	case "-h", "--help", "help":
		fmt.Fprint(e.stdout, "Usage: kingres-verify trust init|show [flags]\n\n  init  write a trust file from a public key you typed or pasted and its fingerprint\n  show  print a trust file and say, for each key, how it stands now and which of its members are enforced\n\nExit status: 0 ok, 2 usage or input error (including a trust file that no command could use).\n")
		return exit(ExitOK)
	}
	fmt.Fprintf(e.stderr, "kingres-verify trust: unknown subcommand %s, expected init, show or follow\n", untrusted(fmt.Sprintf("%q", args[0])))
	return exit(ExitUsage)
}

func (e *env) runTrustInit(args []string) int {
	fs, asJSON := e.newFlagsWithExit("trust init", "--origin-prefix PREFIX (--public-key BASE64 | --public-key-file FILE) --fingerprint HEX [--name LABEL] [--out FILE]",
		"Write a new trust file with one pinned signing key. The public key and the fingerprint must\n"+
			"come from you (typed or pasted from the console), not from the publication being checked:\n"+
			"the command refuses to run without --fingerprint and refuses a key that does not match it.\n"+
			"It does not overwrite a file. --out - writes to the standard output.", "0 written, 2 usage or input error.")
	prefix := fs.String("origin-prefix", "", "origin prefix of the cluster, for example ledger.example.com/kingres")
	pubFlag := fs.String("public-key", "", "Ed25519 public key, base64 of 32 bytes")
	pubFile := fs.String("public-key-file", "", "file holding the base64 public key")
	fpFlag := fs.String("fingerprint", "", "SHA-256 of the public key in hex, as the console shows it")
	name := fs.String("name", "key-1", "label for the key (not the note key name)")
	out := fs.String("out", "trust.json", "file to write, or - for standard output")
	if code, done := e.parseFlags(fs, args); done {
		return code
	}
	const cmd = "trust init"
	if *prefix == "" {
		return e.inputError(cmd, *asJSON, errors.New("missing --origin-prefix"))
	}
	if *fpFlag == "" {
		return e.inputError(cmd, *asJSON, errors.New("missing --fingerprint: type or paste the fingerprint the console shows, so that a wrong key is noticed"))
	}
	if (*pubFlag == "") == (*pubFile == "") {
		return e.inputError(cmd, *asJSON, errors.New("give exactly one of --public-key and --public-key-file"))
	}
	text := *pubFlag
	if *pubFile != "" {
		b, err := readArg(*pubFile, 4096)
		if err != nil {
			return e.inputError(cmd, *asJSON, err)
		}
		text = strings.TrimSpace(string(b))
	}
	pub, err := signednote.DecodeBase64(text)
	if err != nil {
		return e.inputError(cmd, *asJSON, fmt.Errorf("public key: %w", err))
	}
	f, err := trust.New(*prefix, *name, pub, *fpFlag)
	if err != nil {
		return e.inputError(cmd, *asJSON, err)
	}
	data, err := f.Marshal()
	if err != nil {
		return e.inputError(cmd, *asJSON, err)
	}
	if *out == "-" {
		_, _ = e.stdout.Write(data)
		return exit(ExitOK)
	}
	fh, err := os.OpenFile(*out, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644) // #nosec G304 -- the path is a command line argument
	if err != nil {
		return e.inputError(cmd, *asJSON, fmt.Errorf("not writing %s: %w", *out, err))
	}
	// The file was made by this command (O_EXCL), so a failed write takes it away
	// again: a file with nothing in it would make the next run refuse the name.
	if _, err := trustWrite(fh, data); err != nil {
		_ = fh.Close()
		_ = os.Remove(*out)
		return e.inputError(cmd, *asJSON, err)
	}
	if err := fh.Close(); err != nil {
		_ = os.Remove(*out)
		return e.inputError(cmd, *asJSON, err)
	}
	note := "keep this file where the cluster's administrators cannot change it; a trust file that came from the cluster proves nothing"
	if *asJSON {
		e.printJSON(jsonOutput{Command: cmd, Verified: true, Notes: []string{note}, Info: map[string]any{"file": *out, "fingerprint": f.Keys[0].Fingerprint, "origin_prefix": f.OriginPrefix}})
		return exit(ExitOK)
	}
	fmt.Fprintf(e.stdout, "wrote %s: origin prefix %s, key %s (fingerprint %s)\n", untrusted(*out), untrusted(f.OriginPrefix), untrusted(f.Keys[0].Name), f.Keys[0].Fingerprint)
	fmt.Fprintf(e.stdout, "  note: %s\n", note)
	return exit(ExitOK)
}

// trustShowHeading is the verdict word of trust show: it is not a verification
// of anything, so it does not use VERIFIED.
const trustShowHeading = "TRUST FILE"

func (e *env) runTrustShow(args []string) int {
	fs, asJSON := e.newFlagsWithExit("trust show", "--trust FILE [--now TIME] [--json]",
		"Validate and print a trust file, and say for each key how it stands at --now (default: the\n"+
			"system clock) and which of its members are enforced and how. A file that no verifying\n"+
			"command could use (no key may be used now) is reported as such with status 2.", "0 the file is well formed and a key can be used now, 2 usage or input error or no key can be used now.")
	path := fs.String("trust", "", "trust file (JSON)")
	nowFlag := fs.String("now", "", "time for key validity windows, RFC 3339 (default: system clock)")
	if code, done := e.parseFlags(fs, args); done {
		return code
	}
	const cmd = "trust show"
	if *path == "" {
		return e.inputError(cmd, *asJSON, errors.New("missing --trust"))
	}
	data, err := readArg(*path, trust.MaxFileSize)
	if err != nil {
		return e.inputError(cmd, *asJSON, err)
	}
	f, err := trust.Parse(data)
	if err != nil {
		return e.inputError(cmd, *asJSON, fmt.Errorf("trust file %s: %w", *path, err))
	}
	now, fixed, err := parseNow(nowOf(fs, nowFlag))
	if err != nil {
		return e.inputError(cmd, *asJSON, err)
	}
	r := &result{cmd: cmd, word: trustShowHeading}
	if fixed {
		r.notes = append(r.notes, "standing of the keys measured at "+timeText(now)+", fixed by --now")
	} else {
		r.notes = append(r.notes, "standing of the keys measured at the system clock "+timeText(now))
	}
	keys := []map[string]any{}
	usable := 0
	for _, k := range f.Keys {
		st := k.StandingAt(now)
		if st == trust.Usable {
			usable++
		}
		detail := fmt.Sprintf("%q fingerprint %s: %s now", k.Name, k.Fingerprint, st)
		m := map[string]any{"name": k.Name, "fingerprint": k.Fingerprint, "standing": st.String()}
		enforced := map[string]string{}
		if k.NotBefore != nil {
			m["not_before"] = timeText(*k.NotBefore)
			enforced["not_before"] = "enforced: the key is not used before this time (the time itself counts as inside), measured on --now or the system clock"
			detail += " (not_before " + timeText(*k.NotBefore) + ")"
		}
		if k.NotAfter != nil {
			m["not_after"] = timeText(*k.NotAfter)
			enforced["not_after"] = "enforced the same way: the key is not used after this time (the time itself counts as inside), measured on --now or the system clock, never on the time of the signature (a checkpoint has none). The clock is the auditor's: a machine whose clock was set back still accepts the key, so for a key that leaked use revoked, which does not depend on the clock. A checkpoint you already hold (a state file, --old) stays usable after this time. To check older checkpoints give --now an earlier time or use max_tree_size"
			detail += " (not_after " + timeText(*k.NotAfter) + ")"
		}
		if k.Revoked {
			m["revoked"] = true
			enforced["revoked"] = "enforced: the key is never used, whatever --now says"
			detail += " (revoked)"
		}
		if k.MaxTreeSize != nil {
			m["max_tree_size"] = *k.MaxTreeSize
			enforced["max_tree_size"] = fmt.Sprintf("enforced: a checkpoint signed by this key counts only if its tree size is %d or less (the size is in the signed text); this command does not know the size of your log, so a key that can be used here may still be refused for a larger checkpoint", *k.MaxTreeSize)
			detail += fmt.Sprintf(" (max_tree_size %d)", *k.MaxTreeSize)
		}
		m["enforcement"] = enforced
		keys = append(keys, m)
		r.ok("key", detail)
		for _, member := range []string{"not_before", "not_after", "revoked", "max_tree_size"} {
			if why, ok := enforced[member]; ok {
				r.notes = append(r.notes, fmt.Sprintf("key %q, %s: %s", k.Name, member, why))
			}
		}
	}
	r.info = map[string]any{
		"origin_prefix": f.OriginPrefix, "keys": keys, "usable_keys": usable,
		"tsa_roots": len(f.TSARoots), "client_keys": len(f.ClientKeys),
	}
	if len(f.TSARoots) > 0 || len(f.ClientKeys) > 0 {
		r.notes = append(r.notes, fmt.Sprintf("time-stamp authority roots (%d) and client keys (%d) are read and validated but no command uses them yet", len(f.TSARoots), len(f.ClientKeys)))
	}
	r.notes = append(r.notes, "compare the fingerprints with the ones the cluster's console shows, by a channel other than this file")
	if usable == 0 {
		// Every verifying command refuses such a file, so it is not called usable.
		err := fmt.Errorf("trust file %s is well formed for the origin prefix %s but no key can be used at %s: every verifying command will refuse it", *path, f.OriginPrefix, timeText(now))
		return e.finishInputWith(r, *asJSON, err)
	}
	r.headline = fmt.Sprintf("%s is well formed for the origin prefix %s; %d of %d key(s) can be used at %s.", *path, f.OriginPrefix, usable, len(f.Keys), timeText(now))
	return e.finishOK(r, *asJSON)
}

// trustWrite writes the trust file; a test replaces it to make the disk fail.
var trustWrite = func(f *os.File, data []byte) (int, error) { return f.Write(data) }
