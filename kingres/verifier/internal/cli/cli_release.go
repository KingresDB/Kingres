// SPDX-License-Identifier: AGPL-3.0-only

package cli

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/KingresDB/Kingres/kingres/verifier/internal/release"
)

// maxReleaseBinary is the largest binary read for release verify (a build of this
// program is about 8 MiB).
const maxReleaseBinary = 256 << 20

func (e *env) runRelease(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(e.stderr, "kingres-verify release: expected verify")
		return exit(ExitUsage)
	}
	switch args[0] {
	case "verify":
		return e.runReleaseVerify(args[1:])
	case "-h", "--help", "help":
		fmt.Fprint(e.stdout, "Usage: kingres-verify release verify --sums FILE --note FILE --key KEY [--binary FILE]...\n\n  verify  check the signed sums note of a release with the release key, and binaries against its sums\n\nExit status: 0 verified, 1 a check failed, 2 usage or input error.\n")
		return exit(ExitOK)
	}
	fmt.Fprintf(e.stderr, "kingres-verify release: unknown subcommand %s, expected verify\n", untrusted(fmt.Sprintf("%q", args[0])))
	return exit(ExitUsage)
}

// stringList is a flag that may be given more than once.
type stringList []string

func (s *stringList) String() string     { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error { *s = append(*s, v); return nil }

type releaseVerifyArgs struct {
	sums, note, key string
	binaries        []string
	name            string
}

func (e *env) runReleaseVerify(args []string) int {
	fs, asJSON := e.newFlags("release verify", "--sums SHA256SUMS --note SHA256SUMS.note --key KEY [--binary FILE]... [--name NAME]",
		"Check a release of the verifier. The sums note must be a signed note made by the release key you give\n"+
			"(--key: the base64 of the 32 byte public key, or the full key string \"kingres-verify-release+ID+BASE64\"),\n"+
			"and its text must be exactly the sums file. Every --binary is then hashed and compared with the sums\n"+
			"line of the same name (--name gives the name to use when the file was renamed, with one --binary).\n"+
			"The key must come from you (the project's documentation, a printout, a colleague), not from the place\n"+
			"you downloaded the files from: a key that arrived with the files proves nothing.\n"+
			"Nothing is read from the network.")
	sums := fs.String("sums", "", "the SHA256SUMS file")
	note := fs.String("note", "", "the SHA256SUMS.note file (a signed note over the sums)")
	key := fs.String("key", "", "the release key: base64 of the 32 byte Ed25519 public key, or the full verifier key string")
	name := fs.String("name", "", "the name of the release file the (one) --binary is, when the file was renamed")
	var bins stringList
	fs.Var(&bins, "binary", "a binary to check against the sums; may be given more than once")
	if code, done := e.parseFlags(fs, args); done {
		return code
	}
	const cmd = "release verify"
	r := &result{cmd: cmd}
	var o outcome
	if fs.NArg() != 0 {
		o = inputOut(errors.New("unexpected argument " + fmt.Sprintf("%q", fs.Arg(0)) + ": files are given with --sums, --note and --binary"))
	} else {
		o = releaseVerifyRun(r, releaseVerifyArgs{sums: *sums, note: *note, key: *key, binaries: bins, name: *name})
	}
	switch o.kind {
	case outInput:
		return e.inputError(cmd, *asJSON, o.err)
	case outFailed:
		return e.finishFailed(r, *asJSON, o.step, o.err)
	}
	return e.finishOK(r, *asJSON)
}

func releaseVerifyRun(r *result, a releaseVerifyArgs) outcome {
	for _, f := range []struct{ flag, v string }{{"--sums", a.sums}, {"--note", a.note}, {"--key", a.key}} {
		if f.v == "" {
			return inputOut(errors.New("missing " + f.flag))
		}
	}
	if a.name != "" && len(a.binaries) != 1 {
		return inputOut(errors.New("--name needs exactly one --binary"))
	}
	key, err := release.ParseKey(a.key)
	if err != nil {
		return inputOut(err)
	}
	sums, err := readArg(a.sums, release.MaxSums)
	if err != nil {
		return inputOut(err)
	}
	note, err := readArg(a.note, release.MaxSums+8192)
	if err != nil {
		return inputOut(err)
	}
	// The binaries are read before anything is judged, so that an unreadable file is an
	// input error whatever the note says.
	type held struct {
		path string
		data []byte
	}
	var bins []held
	for _, p := range a.binaries {
		b, err := readArg(p, maxReleaseBinary)
		if err != nil {
			return inputOut(err)
		}
		bins = append(bins, held{p, b})
	}
	if err := release.VerifyNote(sums, note, key); err != nil {
		step := "note-signature"
		if errors.Is(err, release.ErrNoteMalformed) {
			step = "note-parse"
		} else if errors.Is(err, release.ErrNoteText) {
			step = "note-text"
		}
		return failedOut(step, err)
	}
	r.ok("note-signature", fmt.Sprintf("the sums note is signed by the release key %s (key id %08x) and its text is the sums file", release.KeyName, key.ID()))
	entries, err := release.ParseSums(sums)
	if err != nil {
		return failedOut("sums-format", err)
	}
	r.ok("sums-format", fmt.Sprintf("the sums list %d file(s): %s", len(entries), listNames(entries)))
	r.info = map[string]any{"key_id": fmt.Sprintf("%08x", key.ID()), "files": len(entries)}
	for _, b := range bins {
		want := a.name
		if want == "" {
			want = filepath.Base(b.path)
		}
		ent, ok := release.Find(entries, want)
		if !ok {
			return failedOut("binary", fmt.Errorf("%q is not in the signed sums (it lists: %s)", want, listNames(entries)))
		}
		got := sha256.Sum256(b.data)
		if !bytes.Equal(got[:], ent.Hash[:]) {
			return failedOut("binary", fmt.Errorf("%q has SHA-256 %s, the signed sums say %s for %s", b.path, hex.EncodeToString(got[:]), hex.EncodeToString(ent.Hash[:]), want))
		}
		r.ok("binary", fmt.Sprintf("%s is %s: SHA-256 %s", b.path, want, hex.EncodeToString(got[:])))
	}
	r.word = "VERIFIED"
	if len(bins) == 0 {
		r.headline = "the sums note is genuine; no binary was given, so none was checked"
		r.notProved = append(r.notProved, "that any binary you hold is a file of this release: give it with --binary")
	} else {
		r.headline = fmt.Sprintf("%d binary(ies) match the sums signed by the release key", len(bins))
	}
	r.notProved = append(r.notProved,
		"that the release key is the project's: it is what you passed with --key, so it is only as good as where you got it",
		"that the binary is free of faults, only that it is the one the key holder signed the hash of",
		"that the source was reviewed: build it yourself from the release tag and compare the SHA-256 (scripts/repro-check.sh shows how)")
	return outcome{kind: outOK}
}

func listNames(es []release.Entry) string {
	var n []string
	for _, e := range es {
		n = append(n, e.Name)
	}
	return strings.Join(n, ", ")
}
