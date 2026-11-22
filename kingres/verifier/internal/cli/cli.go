// SPDX-License-Identifier: AGPL-3.0-only

// Package cli implements the kingres-verify command line on top of the
// library packages. Run is the only entry point; it never calls os.Exit.
package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/KingresDB/Kingres/kingres/verifier/internal/chain"
	"github.com/KingresDB/Kingres/kingres/verifier/internal/format"
	"github.com/KingresDB/Kingres/kingres/verifier/internal/merkle"
	"github.com/KingresDB/Kingres/kingres/verifier/internal/proof"
	"github.com/KingresDB/Kingres/kingres/verifier/internal/rowfile"
	"github.com/KingresDB/Kingres/kingres/verifier/internal/safefile"
)

// Exit statuses.
const (
	// ExitOK means everything that was asked for was verified.
	ExitOK = 0
	// ExitFailed means a verification failed.
	ExitFailed = 1
	// ExitUsage means a usage or input error: nothing was verified.
	ExitUsage = 2
	// ExitIncomplete means nothing failed, but something that was asked for
	// could not be checked with the inputs and flags given.
	ExitIncomplete = 3
	// ExitEvidenceLost means a fork or a rollback was found (what status 1 says)
	// and the conflicting checkpoint could not be saved: the finding stands and
	// the auditor must keep the note they were shown themselves.
	ExitEvidenceLost = 4
	// ExitNotRelease means that `release verify` found the signature and the hashes genuine and the set made by a dry run (the signed RELEASE file says
	// the commit or its date is unknown): a signed build that is not a release. --accept-dry-run turns it into status 0 (P7G-06).
	ExitNotRelease = 5
)

const (
	maxFileSize = 16 << 20
	// maxProofSize is the size of a proof file that is refused: none that a file
	// system holds. A proof carries one header for each block after its block, and
	// is read in pieces (see package proof), so memory does not depend on its size.
	maxProofSize = 1 << 62
	// maxDigestFile bounds one digest file: a digest is a few hundred bytes, and
	// a file of more than 64 KiB is not one.
	maxDigestFile = 64 << 10
	// maxBlocksSize likewise: an export of the block headers is read line by line
	// and one header is kept.
	maxBlocksSize = 1 << 62
	// maxDigestFiles and maxDigestTotal bound what digest-chain reads.
	maxDigestFiles = 1024
	maxDigestTotal = 16 << 20
)

const usageText = `kingres-verify checks the tamper-evident ledger of KingresDB without trusting the database.

Usage:
  kingres-verify <command> [flags]

Commands:
  proof         check an inclusion proof against a trusted digest
  row-hash      compute a row hash from the binary send form of its columns
  digest-chain  check that digests of one chain exported at different times agree
  blocks        recompute the block hash chain from an export of the block headers
  checkpoint    verify a signed checkpoint of a tile log against a trust file
  consistency   check that a log checkpoint extends an earlier one, from tiles
  aggregate     verify the aggregate log of a database against its chain logs
  anchors       verify the anchors (WORM, RFC 3161, OpenTimestamps) of an aggregate checkpoint
  bundle        verify (verify) or copy (fetch) a bundle or a publication
  monitor       watch a publication for a rollback, a fork or a rewrite
  attestation   verify a client attestation of a ledger entry
  release       check the signed sums note of a release, and binaries against it
  trust         create (init), print (show) or follow the key events of (follow) a trust file
  version       print the version

Run "kingres-verify <command> --help" for the flags of a command.
Exit status: 0 verified, 1 verification failed, 2 usage or input error,
3 incomplete (nothing failed, but something asked for could not be checked: an alert, not a pass),
4 a fork or rollback was found and its evidence could not be saved (checkpoint --state).
`

type env struct {
	stdout, stderr io.Writer
	version        string
}

// Run executes the command line args (without the program name) and returns
// the exit status. A write to the standard output that failed (a closed
// descriptor, a full disk) changes status 0 into status 2: 0 says that the
// verdict was printed, and a verdict nobody could read is not that.
func Run(args []string, stdout, stderr io.Writer, version string) int {
	ow := &errWriter{w: stdout}
	code := dispatch(args, ow, stderr, version)
	if ow.err != nil && code == ExitOK {
		fmt.Fprintf(stderr, "kingres-verify: the result could not be written to the standard output (%s): nothing here was reported to you, so do not rely on this run; if --state was given the state file may have been updated\n", untrusted(ow.err.Error()))
		return exit(ExitUsage)
	}
	return code
}

// errWriter remembers the first error of the writer it wraps.
type errWriter struct {
	w   io.Writer
	err error
}

func (e *errWriter) Write(p []byte) (int, error) {
	n, err := e.w.Write(p)
	if err == nil && n < len(p) {
		err = io.ErrShortWrite
	}
	if err != nil && e.err == nil {
		e.err = err
	}
	return n, err
}

func dispatch(args []string, stdout, stderr io.Writer, version string) int {
	e := &env{stdout: stdout, stderr: stderr, version: version}
	if len(args) == 0 {
		fmt.Fprint(stderr, usageText)
		return exit(ExitUsage)
	}
	switch args[0] {
	case "-h", "--help", "help":
		fmt.Fprint(stdout, usageText)
		return exit(ExitOK)
	case "version", "--version":
		fmt.Fprintf(stdout, "kingres-verify %s (hash format version %d)\n", version, format.HashVersion)
		return exit(ExitOK)
	case "proof":
		return e.runProof(args[1:])
	case "row-hash":
		return e.runRowHash(args[1:])
	case "digest-chain":
		return e.runDigestChain(args[1:])
	case "blocks":
		return e.runBlocks(args[1:])
	case "checkpoint":
		return e.runCheckpoint(args[1:])
	case "consistency":
		return e.runConsistency(args[1:])
	case "trust":
		return e.runTrust(args[1:])
	case "aggregate":
		return e.runAggregate(args[1:])
	case "anchors":
		return e.runAnchors(args[1:])
	case "bundle":
		return e.runBundle(args[1:])
	case "monitor":
		return e.runMonitor(args[1:])
	case "attestation":
		return e.runAttestation(args[1:])
	case "release":
		return e.runRelease(args[1:])
	}
	fmt.Fprintf(stderr, "kingres-verify: unknown command %s, expected one of proof, row-hash, digest-chain, blocks, checkpoint, consistency, aggregate, anchors, bundle, monitor, attestation, release, trust, version\n", untrusted(fmt.Sprintf("%q", args[0])))
	return exit(ExitUsage)
}

// newFlags creates a flag set that prints its help to stdout.
func (e *env) newFlags(name, synopsis, help string) (*flag.FlagSet, *bool) {
	return e.newFlagsWithExit(name, synopsis, help, "0 verified, 1 verification failed, 2 usage or input error.")
}

// newFlagsWithExit is newFlags with its own text for the exit statuses.
func (e *env) newFlagsWithExit(name, synopsis, help, exitText string) (*flag.FlagSet, *bool) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	jsonOut := fs.Bool("json", false, "print the result as JSON")
	fs.Usage = func() {
		fmt.Fprintf(e.stdout, "Usage: kingres-verify %s %s\n\n%s\n\nFlags:\n", name, synopsis, help)
		fs.SetOutput(e.stdout)
		fs.PrintDefaults()
		fs.SetOutput(io.Discard)
		fmt.Fprint(e.stdout, "\nExit status: "+exitText+"\n")
	}
	return fs, jsonOut
}

// parseFlags returns done=true with the exit status when the command must
// stop (help or a flag error).
func (e *env) parseFlags(fs *flag.FlagSet, args []string) (code int, done bool) {
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exit(ExitOK), true
		}
		fmt.Fprintf(e.stderr, "kingres-verify %s: %s\nRun \"kingres-verify %s --help\".\n", fs.Name(), untrusted(err.Error()), fs.Name())
		return exit(ExitUsage), true
	}
	return 0, false
}

// readArg reads a file named on the command line: never blocking, a regular
// file after links are resolved, at most limit bytes (package safefile).
func readArg(path string, limit int64) ([]byte, error) { return safefile.ReadArg(path, limit) }

func requireFlag(name, val string) error {
	if val == "" {
		return fmt.Errorf("missing --%s", name)
	}
	return nil
}

// ---- proof ----

func (e *env) runProof(args []string) int {
	fs, asJSON := e.newFlags("proof", "--proof FILE --digest FILE [--table-id N] [--row-hash HEX] [--op I|D|any] [--json]",
		"Check an inclusion proof against a trusted digest of the same chain, following the\n"+
			"verification steps of the ledger README. With --table-id and --row-hash the proof must\n"+
			"also be for that table and row (recompute the row hash with the row-hash command).\n"+
			"A proof is for an entry of operation I (this version of the row was inserted) or D\n"+
			"(this version was removed, by an update or a delete): with --op I or --op D the proof\n"+
			"must be for that operation, with the default --op any either is accepted and the\n"+
			"output says which one was proven.")
	proofPath := fs.String("proof", "", "inclusion proof file (JSON)")
	digestPath := fs.String("digest", "", "trusted digest file (JSON)")
	tableID := fs.Int64("table-id", -1, "expected table id of the proof")
	rowHash := fs.String("row-hash", "", "expected row hash, 64 lower case hex characters")
	opFlag := fs.String("op", "any", "expected operation of the entry: I, D or any")
	bundle := fs.String("bundle", "", "anchor the proof in a signed log instead of a digest: the publication or bundle (directory, .tar.gz or https address)")
	logLeg := fs.String("log-leg", "", "with --bundle: chain (the signed chain log) or aggregate (the signed aggregate log)")
	trustPath := fs.String("trust", "", "with --bundle: the trust file")
	epoch := fs.Uint64("epoch", 0, "with --bundle: the epoch of the chain log (default: the only one in the source)")
	aggID := fs.String("aggregate-id", "", "with --bundle --log-leg aggregate: the aggregate id (default: the only one in the source)")
	nowFlag := fs.String("now", "", "with --bundle: time for key validity windows, RFC 3339 (default: system clock)")
	var acceptEpoch logListFlag
	fs.Var(&acceptEpoch, "accept-epoch-closed", "with --bundle --log-leg aggregate: the number of an epoch-closed event whose finding you accept (repeatable)")
	if code, done := e.parseFlags(fs, args); done {
		return code
	}
	if *bundle != "" {
		return e.runProofLeg(*asJSON, proofLegArgs{proofPath: *proofPath, digest: *digestPath, bundle: *bundle, leg: *logLeg, trust: *trustPath, aggID: *aggID, epoch: *epoch,
			tableID: *tableID, rowHash: *rowHash, op: *opFlag, acceptEpoch: acceptEpoch, now: nowOf(fs, nowFlag)})
	}
	const cmd = "proof"
	for _, c := range [][2]string{{"proof", *proofPath}, {"digest", *digestPath}} {
		if err := requireFlag(c[0], c[1]); err != nil {
			return e.inputError(cmd, *asJSON, err)
		}
	}
	var exp proof.Expect
	if *tableID != -1 {
		if *tableID < 0 || *tableID > chain.MaxInt4 {
			return e.inputError(cmd, *asJSON, fmt.Errorf("--table-id %d is out of range, expected 0 to %d", *tableID, chain.MaxInt4))
		}
		t := uint32(*tableID) // #nosec G115 -- range checked
		exp.TableID = &t
	}
	if *rowHash != "" {
		h, err := format.DecodeHash(*rowHash)
		if err != nil {
			return e.inputError(cmd, *asJSON, fmt.Errorf("--row-hash: %w", err))
		}
		exp.RowHash = &h
	}
	switch *opFlag {
	case "any":
	case "I":
		op := format.OpInsert
		exp.Op = &op
	case "D":
		op := format.OpDelete
		exp.Op = &op
	default:
		return e.inputError(cmd, *asJSON, fmt.Errorf("--op %q is not I, D or any", *opFlag))
	}
	digestData, err := readArg(*digestPath, maxDigestFile)
	if err != nil {
		return e.inputError(cmd, *asJSON, err)
	}
	d, err := chain.ParseDigest(digestData)
	if err != nil {
		return e.inputError(cmd, *asJSON, err)
	}
	// The proof is read from the file in pieces and twice (package proof): its
	// size is not limited, because a proof of an old row is large, and its memory
	// does not depend on its size.
	open := func() (io.ReadCloser, error) { return safefile.OpenArg(*proofPath, maxProofSize) }
	probe, err := open()
	if err != nil {
		return e.inputError(cmd, *asJSON, err)
	}
	_ = probe.Close()
	r := &result{cmd: cmd}
	p, sum, err := proof.ParseStream(open)
	if err != nil {
		return e.finishFailed(r, *asJSON, proof.StepParse, err)
	}
	res, err := proof.VerifyStream(p, sum, d, exp, open)
	if res != nil {
		for _, s := range res.Steps {
			r.ok(s.Name, s.Detail)
		}
	}
	if err != nil {
		var pe *proof.Error
		step := ""
		if errors.As(err, &pe) {
			step = pe.Step
			err = pe.Err
		}
		return e.finishFailed(r, *asJSON, step, err)
	}
	r.ok("entry", fmt.Sprintf("table_id %d, row_hash %x, written by %q in block %d", p.TableID, p.RowHash, p.UserName, p.Block.Height))
	r.ok("operation", proof.OpName(p.Op)+": "+opMeaning(p.Op))
	r.headline = fmt.Sprintf("the proof is valid against the digest at height %d.", d.Height)
	r.info = map[string]any{
		"table_id":      p.TableID,
		"op":            proof.OpName(p.Op),
		"proves":        opMeaning(p.Op),
		"row_hash":      format.EncodeHex(p.RowHash[:]),
		"block_height":  p.Block.Height,
		"digest_height": d.Height,
		"user_name":     p.UserName,
	}
	bound := exp.TableID != nil && exp.RowHash != nil
	if !bound {
		r.notProved = append(r.notProved, "no --table-id and --row-hash were given: this shows that an entry with this row_hash exists in the ledger, not that it is the row you hold")
	}
	r.notProved = append(r.notProved, proofLimits...)
	return e.finishOK(r, *asJSON)
}

// opMeaning says what a proof of an entry of this operation proves; the two
// are different statements and a reader must not mix them up.
func opMeaning(op byte) string {
	if op == format.OpDelete {
		return "a version of a row with this hash was removed (updated away or deleted) in this transaction"
	}
	return "a version of a row with this hash was inserted (entered) in this transaction"
}

var proofLimits = []string{
	"this holds only if the digest was obtained and kept outside the database's control",
	"the row hash must be recomputed from data you hold (kingres-verify row-hash)",
}

// ---- row-hash ----

func (e *env) runRowHash(args []string) int {
	fs, asJSON := e.newFlags("row-hash", "--table-id N --columns FILE [--encoding] [--json]",
		"Compute the row hash of a row from the binary send form of its non-NULL columns.\n"+
			"The columns file is a JSON array in column order, for example\n"+
			`  [{"name": "id", "send_hex": "00000001"}, {"name": "note", "send_hex": "6869"}]`+"\n"+
			"where send_hex is the lower case hex of the bytes of the type's binary send function.")
	tableID := fs.Int64("table-id", -1, "table id of the ledger table (the table_id of the proof)")
	columns := fs.String("columns", "", "columns file (JSON)")
	showEnc := fs.Bool("encoding", false, "also print the canonical row encoding as hex")
	if code, done := e.parseFlags(fs, args); done {
		return code
	}
	const cmd = "row-hash"
	if *tableID < 0 || *tableID > chain.MaxInt4 {
		return e.inputError(cmd, *asJSON, fmt.Errorf("--table-id is required and must be 0 to %d, got %d", chain.MaxInt4, *tableID))
	}
	if err := requireFlag("columns", *columns); err != nil {
		return e.inputError(cmd, *asJSON, err)
	}
	data, err := readArg(*columns, maxFileSize)
	if err != nil {
		return e.inputError(cmd, *asJSON, err)
	}
	cols, err := rowfile.Parse(data)
	if err != nil {
		return e.inputError(cmd, *asJSON, fmt.Errorf("columns file: %w", err))
	}
	t := uint32(*tableID) // #nosec G115 -- range checked
	enc, err := format.EncodeRow(t, cols)
	if err != nil {
		return e.inputError(cmd, *asJSON, err)
	}
	h := merkle.LeafHash(enc)
	if *asJSON {
		info := map[string]any{"table_id": t, "row_hash": format.EncodeHex(h[:])}
		if *showEnc {
			info["encoding"] = format.EncodeHex(enc)
		}
		e.printJSON(jsonOutput{Command: cmd, Verified: false, Info: info, Notes: []string{"row-hash computes a hash and verifies nothing: verified is false by design, use proof to check an entry"}})
		return exit(ExitOK)
	}
	fmt.Fprintf(e.stdout, "%x\n", h)
	if *showEnc {
		fmt.Fprintf(e.stdout, "encoding %x\n", enc)
	}
	return exit(ExitOK)
}

// ---- digest-chain ----

type listFlag []string

func (l *listFlag) String() string { return strings.Join(*l, ",") }
func (l *listFlag) Set(v string) error {
	*l = append(*l, v)
	return nil
}

func (e *env) runDigestChain(args []string) int {
	fs, asJSON := e.newFlags("digest-chain", "--digests DIR|FILE [--digests FILE ...] [FILE ...] [--json]",
		"Check that digests of one chain, exported at different times, are mutually consistent:\n"+
			"same chain id, block times and generation order that do not go back, and equal block\n"+
			"hashes for equal heights. A directory stands for its *.json files.")
	var paths listFlag
	fs.Var(&paths, "digests", "a digest file or a directory of digest files (repeatable)")
	if code, done := e.parseFlags(fs, args); done {
		return code
	}
	const cmd = "digest-chain"
	paths = append(paths, fs.Args()...)
	if len(paths) == 0 {
		return e.inputError(cmd, *asJSON, errors.New("missing --digests"))
	}
	// One digest is a few hundred bytes. The number of files and their total
	// size are bounded so that a directory the publisher filled cannot use the
	// auditor's memory; a directory is listed by name, never as a glob pattern
	// (a directory called x[ab] is that directory).
	var ds []chain.NamedDigest
	var total int64
	add := func(name string, data []byte) error {
		if len(ds) >= maxDigestFiles {
			return fmt.Errorf("more than %d digest files: give fewer, or the ones you need", maxDigestFiles)
		}
		if total += int64(len(data)); total > maxDigestTotal {
			return fmt.Errorf("the digest files are larger than %d bytes together", maxDigestTotal)
		}
		d, err := chain.ParseDigest(data)
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		ds = append(ds, chain.NamedDigest{Name: name, Digest: d})
		return nil
	}
	for _, p := range paths {
		st, err := os.Stat(p)
		if err != nil {
			return e.inputError(cmd, *asJSON, err)
		}
		if !st.IsDir() {
			data, err := readArg(p, maxDigestFile)
			if err == nil {
				err = add(p, data)
			}
			if err != nil {
				return e.inputError(cmd, *asJSON, err)
			}
			continue
		}
		dir, err := safefile.OpenDir(p)
		if err != nil {
			return e.inputError(cmd, *asJSON, err)
		}
		names, others, err := dir.RegularNames()
		for _, n := range others {
			if err == nil && strings.HasSuffix(n, ".json") {
				err = fmt.Errorf("%s %w", filepath.Join(p, n), safefile.ErrNotRegular)
			}
		}
		for _, n := range names {
			if err != nil {
				break
			}
			if !strings.HasSuffix(n, ".json") {
				continue
			}
			var data []byte
			if data, err = dir.ReadEntry(n, maxDigestFile); err == nil {
				err = add(filepath.Join(p, n), data)
			}
		}
		_ = dir.Close()
		if err != nil {
			return e.inputError(cmd, *asJSON, err)
		}
	}
	if len(ds) == 0 {
		// A directory with no *.json file would otherwise be reported as
		// consistent: nothing was compared.
		return e.inputError(cmd, *asJSON, errors.New("no digest file found: a directory stands for its *.json files and none was there"))
	}
	r := &result{cmd: cmd, word: "CONSISTENT", info: map[string]any{"digests": len(ds)}}
	r.problems = chain.CheckDigestSet(ds)
	if len(r.problems) > 0 {
		return e.finishProblems(r, *asJSON)
	}
	r.headline = fmt.Sprintf("%d digests agree with each other.", len(ds))
	r.notProved = []string{
		"this compares digests only: it does not prove that the blocks between them are intact (use the blocks command or a proof)",
		"it does not prove any row or that any digest is authentic: each digest must have been stored outside the database's control",
		"generated_at is not authenticated; block times are the database server's clock",
	}
	return e.finishOK(r, *asJSON)
}

// ---- blocks ----

func (e *env) runBlocks(args []string) int {
	fs, asJSON := e.newFlags("blocks", "--blocks FILE (--digest FILE | --chain-id HEX) [--json]",
		"Recompute every block hash of an export of the block headers (one JSON object per line)\n"+
			"from the genesis hash of the chain: dense heights, block times that never go back and,\n"+
			"with --digest, a chain that reaches the digest. The chain id comes from --digest or\n"+
			"--chain-id.")
	blocksPath := fs.String("blocks", "", "block header export (one JSON object per line)")
	digestPath := fs.String("digest", "", "trusted digest file (JSON); the chain must reach it")
	chainHex := fs.String("chain-id", "", "chain id, 64 lower case hex characters (when no digest is given)")
	if code, done := e.parseFlags(fs, args); done {
		return code
	}
	const cmd = "blocks"
	if err := requireFlag("blocks", *blocksPath); err != nil {
		return e.inputError(cmd, *asJSON, err)
	}
	var cid [32]byte
	var d chain.Digest
	haveDigest := *digestPath != ""
	switch {
	case haveDigest:
		data, err := readArg(*digestPath, maxDigestFile)
		if err != nil {
			return e.inputError(cmd, *asJSON, err)
		}
		if d, err = chain.ParseDigest(data); err != nil {
			return e.inputError(cmd, *asJSON, err)
		}
		cid = d.ChainID
		if *chainHex != "" {
			given, err := format.DecodeHash(*chainHex)
			if err != nil {
				return e.inputError(cmd, *asJSON, fmt.Errorf("--chain-id: %w", err))
			}
			if given != cid {
				return e.inputError(cmd, *asJSON, errors.New("--chain-id differs from the chain_id of --digest, expected the same chain"))
			}
		}
	case *chainHex != "":
		var err error
		if cid, err = format.DecodeHash(*chainHex); err != nil {
			return e.inputError(cmd, *asJSON, fmt.Errorf("--chain-id: %w", err))
		}
	default:
		return e.inputError(cmd, *asJSON, errors.New("missing --digest or --chain-id: the genesis hash needs the chain id"))
	}
	f, err := safefile.OpenArg(*blocksPath, maxBlocksSize)
	if err != nil {
		return e.inputError(cmd, *asJSON, err)
	}
	defer f.Close()
	r := &result{cmd: cmd}
	// The export is read line by line and one header is kept: memory does not
	// grow with the file.
	var dp *chain.Digest
	if haveDigest {
		dp = &d
	}
	stream, err := chain.StreamBlocks(f, cid, dp)
	if err != nil {
		var se *chain.StreamError
		if errors.As(err, &se) {
			if errors.Is(se.Err, safefile.ErrTooLarge) {
				return e.inputError(cmd, *asJSON, se.Err)
			}
			return e.finishFailed(r, *asJSON, se.Step, se.Err)
		}
		return e.inputError(cmd, *asJSON, err)
	}
	head := stream.Head
	r.ok("chain", fmt.Sprintf("%d blocks, heights 0 to %d, recomputed from the genesis hash; the last block read is %d, hash %x", stream.Blocks, head.Height, head.Height, head.BlockHash))
	r.headline = fmt.Sprintf("the %d block headers read (heights 0 to %d) form an unbroken chain from the genesis hash.", stream.Blocks, head.Height)
	r.info = map[string]any{
		"blocks": stream.Blocks, "head_height": head.Height, "head_hash": format.EncodeHex(head.BlockHash[:]),
		"complete_export_shown": false,
		// anchored_to_digest is what a script tests: false means that nothing
		// ties this chain to a digest kept outside the database, so a chain that
		// was rewritten from the genesis block verifies too (status 0 says that the
		// headers form a chain, not that the chain is the one that was published).
		"anchored_to_digest": haveDigest,
	}
	if haveDigest {
		r.ok("digest", fmt.Sprintf("the chain reaches the digest at height %d", d.Height))
		r.headline = fmt.Sprintf("the %d block headers read (heights 0 to %d) form an unbroken chain from the genesis hash and reach the digest at height %d.", stream.Blocks, head.Height, d.Height)
		r.info["digest_height"] = d.Height
	}
	r.notProved = []string{"this checks the exported headers only: not any transaction, row or table, which needs inclusion proofs"}
	// What an export that ends proves is what it holds: a cut after any block
	// verifies the same way, because the hash of a block does not say whether
	// another follows. Only a digest kept outside the database ties an end.
	if !haveDigest {
		r.notProved = append(r.notProved,
			fmt.Sprintf("that the export is complete: it ends at block %d, hash %x, and an export cut at any earlier block verifies the same way; to show that this is the head, give --digest of a height at or above %d, or compare the hash with the head you were told", head.Height, head.BlockHash, head.Height),
			"no --digest was given: nothing ties this chain to a digest kept outside the database, so a chain that was rewritten from the genesis block verifies too")
	} else {
		r.notProved = append(r.notProved,
			fmt.Sprintf("that the export is complete: it holds blocks up to %d, and the digest covers up to height %d; blocks after the digest height are not covered, and an export cut after the digest height verifies the same way", head.Height, d.Height),
			"the digest must have been kept outside the database's control")
	}
	return e.finishOK(r, *asJSON)
}
