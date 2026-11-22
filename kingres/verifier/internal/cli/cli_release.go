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
	acceptDryRun    bool
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
	acceptDryRun := fs.Bool("accept-dry-run", false, "exit 0 for a set that a dry run signed (the signed RELEASE file says the commit is unknown); without it such a set ends with status 5, and the verdict says NOT A RELEASE in both cases")
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
		o = releaseVerifyRun(r, releaseVerifyArgs{sums: *sums, note: *note, key: *key, binaries: bins, name: *name, acceptDryRun: *acceptDryRun})
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
	shown := false       // the RELEASE file was given and its content is shown
	notARelease := false // the signed RELEASE file says the commit or its date is unknown
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
		if want == release.InfoName {
			// the file that says which release this is: its hash is in the signed sums, so what it says is signed
			info, err := release.ParseInfo(b.data)
			if err != nil {
				return failedOut("release-info", fmt.Errorf("%q matches the signed sums and is not a RELEASE file of this format: %w", b.path, err))
			}
			shown = true
			r.ok("release-info", fmt.Sprintf("the signed release file says: version %s, commit %s, commit date %s, built with %s", info.Version, info.Commit, info.Date, info.Go))
			r.info["release_version"], r.info["release_commit"], r.info["release_date"], r.info["release_go"] = info.Version, info.Commit, info.Date, info.Go
			if info.Commit == "unknown" || info.Date == "unknown" {
				// release.sh --dry-run signs a set from a tree that is dirty or not at a tag and says so here (P7F-04): the signature is genuine and the set is no release
				notARelease = true
				r.info["not_a_release"] = true
			}
		}
	}
	r.word = "VERIFIED"
	if len(bins) == 0 {
		r.headline = "the sums note is genuine; no binary was given, so none was checked"
		r.notProved = append(r.notProved, "that any binary you hold is a file of this release: give it with --binary")
	} else {
		r.headline = fmt.Sprintf("%d binary(ies) match the sums signed by the release key", len(bins))
	}
	if notARelease {
		r.word = "VERIFIED (NOT A RELEASE)"
		if !a.acceptDryRun {
			r.exit = ExitNotRelease
		}
		r.headline += "; BUT the signed RELEASE file says the commit is unknown: this set was made by a dry run, from a checkout that was not clean or not at a release tag, so it is a signed build and not a release"
		r.notes = append(r.notes, "finding: the signed RELEASE file records commit or date \"unknown\" (scripts/release.sh --dry-run). The signature is the release key's and the hashes match, and that is all the run shows: the set was not made from a clean tagged checkout, it must not be taken for a release and it cannot be reproduced from a tag")
	}
	if _, listed := release.Find(entries, release.InfoName); listed && !shown {
		r.notProved = append(r.notProved, "which release this is: the signed sums list a "+release.InfoName+" file with the version, commit and date; give it with --binary "+release.InfoName+" to see them")
	} else if !listed {
		r.notProved = append(r.notProved, "which release this is: the signed sums list no "+release.InfoName+" file (a release made before releases signed their version, or a build that is not a release)")
	}
	r.notProved = append(r.notProved,
		"that this is the newest release: a genuine older release, with a known fault, verifies as well, and the signed files do not expire (a rollback to an older release cannot be seen from the files); compare the version above with the project's announcement of the newest release, which you must get from the project and not from the place the files came from",
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
