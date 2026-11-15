// SPDX-License-Identifier: PostgreSQL

// Package cli implements the kingres-verify command line on top of the
// library packages. Run is the only entry point; it never calls os.Exit.
package cli

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/KingresDB/Kingres/kingres/verifier/internal/chain"
	"github.com/KingresDB/Kingres/kingres/verifier/internal/format"
	"github.com/KingresDB/Kingres/kingres/verifier/internal/merkle"
	"github.com/KingresDB/Kingres/kingres/verifier/internal/proof"
	"github.com/KingresDB/Kingres/kingres/verifier/internal/rowfile"
)

// Exit statuses.
const (
	// ExitOK means everything that was asked for was verified.
	ExitOK = 0
	// ExitFailed means a verification failed.
	ExitFailed = 1
	// ExitUsage means a usage or input error: nothing was verified.
	ExitUsage = 2
)

const (
	maxFileSize   = 16 << 20
	maxBlocksSize = 1 << 30
)

const usageText = `kingres-verify checks the tamper-evident ledger of KingresDB without trusting the database.

Usage:
  kingres-verify <command> [flags]

Commands:
  proof         check an inclusion proof against a trusted digest
  row-hash      compute a row hash from the binary send form of its columns
  digest-chain  check that digests of one chain exported at different times agree
  blocks        recompute the block hash chain from an export of the block headers
  version       print the version

Run "kingres-verify <command> --help" for the flags of a command.
Exit status: 0 verified, 1 verification failed, 2 usage or input error.
`

type env struct {
	stdout, stderr io.Writer
	version        string
}

// Run executes the command line args (without the program name) and returns
// the exit status.
func Run(args []string, stdout, stderr io.Writer, version string) int {
	e := &env{stdout: stdout, stderr: stderr, version: version}
	if len(args) == 0 {
		fmt.Fprint(stderr, usageText)
		return ExitUsage
	}
	switch args[0] {
	case "-h", "--help", "help":
		fmt.Fprint(stdout, usageText)
		return ExitOK
	case "version", "--version":
		fmt.Fprintf(stdout, "kingres-verify %s (hash format version %d)\n", version, format.HashVersion)
		return ExitOK
	case "proof":
		return e.runProof(args[1:])
	case "row-hash":
		return e.runRowHash(args[1:])
	case "digest-chain":
		return e.runDigestChain(args[1:])
	case "blocks":
		return e.runBlocks(args[1:])
	}
	fmt.Fprintf(stderr, "kingres-verify: unknown command %q, expected one of proof, row-hash, digest-chain, blocks, version\n", args[0])
	return ExitUsage
}

// newFlags creates a flag set that prints its help to stdout.
func (e *env) newFlags(name, synopsis, help string) (*flag.FlagSet, *bool) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	jsonOut := fs.Bool("json", false, "print the result as JSON")
	fs.Usage = func() {
		fmt.Fprintf(e.stdout, "Usage: kingres-verify %s %s\n\n%s\n\nFlags:\n", name, synopsis, help)
		fs.SetOutput(e.stdout)
		fs.PrintDefaults()
		fs.SetOutput(io.Discard)
		fmt.Fprint(e.stdout, "\nExit status: 0 verified, 1 verification failed, 2 usage or input error.\n")
	}
	return fs, jsonOut
}

// parseFlags returns done=true with the exit status when the command must
// stop (help or a flag error).
func (e *env) parseFlags(fs *flag.FlagSet, args []string) (code int, done bool) {
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return ExitOK, true
		}
		fmt.Fprintf(e.stderr, "kingres-verify %s: %v\nRun \"kingres-verify %s --help\".\n", fs.Name(), err, fs.Name())
		return ExitUsage, true
	}
	return 0, false
}

// jsonOut is the JSON form of every result.
type jsonOutput struct {
	Command   string     `json:"command"`
	Verified  bool       `json:"verified"`
	Steps     []stepJSON `json:"steps,omitempty"`
	Problems  []string   `json:"problems,omitempty"`
	FailedAt  string     `json:"failed_step,omitempty"`
	Error     string     `json:"error,omitempty"`
	InputErr  bool       `json:"input_error,omitempty"`
	Info      any        `json:"info,omitempty"`
	NotProved []string   `json:"not_proved,omitempty"`
}

type stepJSON struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail,omitempty"`
}

func (e *env) printJSON(v any) {
	enc := json.NewEncoder(e.stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

// inputError reports a usage or input problem (exit status 2).
func (e *env) inputError(cmd string, asJSON bool, err error) int {
	if asJSON {
		e.printJSON(jsonOutput{Command: cmd, InputErr: true, Error: err.Error()})
	}
	fmt.Fprintf(e.stderr, "kingres-verify %s: %v\n", cmd, err)
	return ExitUsage
}

func readLimited(path string, limit int64) ([]byte, error) {
	f, err := os.Open(path) // #nosec G304 -- the path is a command line argument
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("%s is larger than %d bytes", path, limit)
	}
	return data, nil
}

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
	if code, done := e.parseFlags(fs, args); done {
		return code
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
	digestData, err := readLimited(*digestPath, maxFileSize)
	if err != nil {
		return e.inputError(cmd, *asJSON, err)
	}
	d, err := chain.ParseDigest(digestData)
	if err != nil {
		return e.inputError(cmd, *asJSON, err)
	}
	proofData, err := readLimited(*proofPath, maxFileSize)
	if err != nil {
		return e.inputError(cmd, *asJSON, err)
	}
	out := jsonOutput{Command: cmd}
	p, err := proof.Parse(proofData)
	if err != nil {
		return e.failProof(out, *asJSON, proof.StepParse, err, nil)
	}
	res, err := proof.Verify(p, d, exp)
	if err != nil {
		var pe *proof.Error
		step := ""
		if errors.As(err, &pe) {
			step = pe.Step
			err = pe.Err
		}
		return e.failProof(out, *asJSON, step, err, res)
	}
	for _, s := range res.Steps {
		out.Steps = append(out.Steps, stepJSON{Name: s.Name, OK: true, Detail: s.Detail})
	}
	out.Verified = true
	out.Info = map[string]any{
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
		out.NotProved = append(out.NotProved, "no --table-id and --row-hash were given: this shows that an entry with this row_hash exists in the ledger, not that it is the row you hold")
	}
	out.NotProved = append(out.NotProved, proofLimits...)
	if *asJSON {
		e.printJSON(out)
		return ExitOK
	}
	fmt.Fprintf(e.stdout, "VERIFIED: the proof is valid against the digest at height %d.\n", d.Height)
	for _, s := range res.Steps {
		fmt.Fprintf(e.stdout, "  ok  %-12s %s\n", s.Name, s.Detail)
	}
	fmt.Fprintf(e.stdout, "  table_id %d, row_hash %x, written by %q in block %d\n", p.TableID, p.RowHash, p.UserName, p.Block.Height)
	fmt.Fprintf(e.stdout, "  operation %s: %s\n", proof.OpName(p.Op), opMeaning(p.Op))
	for _, n := range out.NotProved {
		fmt.Fprintf(e.stdout, "  note: %s\n", n)
	}
	return ExitOK
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

func (e *env) failProof(out jsonOutput, asJSON bool, step string, err error, res *proof.Result) int {
	out.FailedAt = step
	out.Error = err.Error()
	if res != nil {
		for _, s := range res.Steps {
			out.Steps = append(out.Steps, stepJSON{Name: s.Name, OK: true, Detail: s.Detail})
		}
	}
	out.Steps = append(out.Steps, stepJSON{Name: step, OK: false, Detail: err.Error()})
	if asJSON {
		e.printJSON(out)
		return ExitFailed
	}
	fmt.Fprintf(e.stdout, "FAILED at step %s: %v\n", step, err)
	return ExitFailed
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
	data, err := readLimited(*columns, maxFileSize)
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
		e.printJSON(jsonOutput{Command: cmd, Verified: true, Info: info})
		return ExitOK
	}
	fmt.Fprintf(e.stdout, "%x\n", h)
	if *showEnc {
		fmt.Fprintf(e.stdout, "encoding %x\n", enc)
	}
	return ExitOK
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
	var files []string
	for _, p := range paths {
		st, err := os.Stat(p)
		if err != nil {
			return e.inputError(cmd, *asJSON, err)
		}
		if !st.IsDir() {
			files = append(files, p)
			continue
		}
		m, err := filepath.Glob(filepath.Join(p, "*.json"))
		if err != nil {
			return e.inputError(cmd, *asJSON, err)
		}
		sort.Strings(m)
		files = append(files, m...)
	}
	var ds []chain.NamedDigest
	for _, f := range files {
		data, err := readLimited(f, maxFileSize)
		if err != nil {
			return e.inputError(cmd, *asJSON, err)
		}
		d, err := chain.ParseDigest(data)
		if err != nil {
			return e.inputError(cmd, *asJSON, fmt.Errorf("%s: %w", f, err))
		}
		ds = append(ds, chain.NamedDigest{Name: f, Digest: d})
	}
	problems := chain.CheckDigestSet(ds)
	notProved := []string{
		"this compares digests only: it does not prove that the blocks between them are intact (use the blocks command or a proof)",
		"it does not prove any row or that any digest is authentic: each digest must have been stored outside the database's control",
		"generated_at is not authenticated; block times are the database server's clock",
	}
	if len(problems) > 0 {
		if *asJSON {
			e.printJSON(jsonOutput{Command: cmd, Problems: problems})
			return ExitFailed
		}
		fmt.Fprintln(e.stdout, "INCONSISTENT:")
		for _, p := range problems {
			fmt.Fprintf(e.stdout, "  - %s\n", p)
		}
		return ExitFailed
	}
	if *asJSON {
		e.printJSON(jsonOutput{Command: cmd, Verified: true, NotProved: notProved, Info: map[string]any{"digests": len(ds)}})
		return ExitOK
	}
	fmt.Fprintf(e.stdout, "CONSISTENT: %d digests agree with each other.\n", len(ds))
	for _, n := range notProved {
		fmt.Fprintf(e.stdout, "  note: %s\n", n)
	}
	return ExitOK
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
		data, err := readLimited(*digestPath, maxFileSize)
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
	data, err := readLimited(*blocksPath, maxBlocksSize)
	if err != nil {
		return e.inputError(cmd, *asJSON, err)
	}
	out := jsonOutput{Command: cmd}
	headers, err := chain.ParseBlocks(data)
	if err != nil {
		return e.failBlocks(out, *asJSON, "parse", err)
	}
	if err := chain.VerifyBlocks(headers, cid); err != nil {
		return e.failBlocks(out, *asJSON, "chain", err)
	}
	out.Steps = append(out.Steps, stepJSON{Name: "chain", OK: true, Detail: fmt.Sprintf("%d blocks, heights 0 to %d, recomputed from the genesis hash", len(headers), len(headers)-1)})
	if haveDigest {
		if err := chain.CheckDigestOnBlocks(headers, d); err != nil {
			return e.failBlocks(out, *asJSON, "digest", err)
		}
		out.Steps = append(out.Steps, stepJSON{Name: "digest", OK: true, Detail: fmt.Sprintf("the chain reaches the digest at height %d", d.Height)})
	}
	out.Verified = true
	out.NotProved = []string{"this checks the exported headers only: not any transaction, row or table, which needs inclusion proofs"}
	if !haveDigest {
		out.NotProved = append(out.NotProved, "no --digest was given: nothing ties this chain to a digest kept outside the database")
	}
	if *asJSON {
		e.printJSON(out)
		return ExitOK
	}
	fmt.Fprintln(e.stdout, "VERIFIED: the block chain is consistent.")
	for _, s := range out.Steps {
		fmt.Fprintf(e.stdout, "  ok  %-8s %s\n", s.Name, s.Detail)
	}
	for _, n := range out.NotProved {
		fmt.Fprintf(e.stdout, "  note: %s\n", n)
	}
	return ExitOK
}

func (e *env) failBlocks(out jsonOutput, asJSON bool, step string, err error) int {
	out.FailedAt = step
	out.Error = err.Error()
	out.Steps = append(out.Steps, stepJSON{Name: step, OK: false, Detail: err.Error()})
	if asJSON {
		e.printJSON(out)
		return ExitFailed
	}
	fmt.Fprintf(e.stdout, "FAILED at step %s: %v\n", step, err)
	return ExitFailed
}
