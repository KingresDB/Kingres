// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

// Package proof parses ledger inclusion proofs and verifies them against a
// trusted digest, following the verification steps of
// contrib/kingres_ledger/README.md, section "What a proof proves, and what the
// verifier must do".
package proof

import (
	"errors"
	"fmt"
	"math"

	"github.com/KingresDB/Kingres/kingres/verifier/internal/chain"
	"github.com/KingresDB/Kingres/kingres/verifier/internal/format"
	"github.com/KingresDB/Kingres/kingres/verifier/internal/jsonx"
	"github.com/KingresDB/Kingres/kingres/verifier/internal/merkle"
)

// Names of the verification steps, in the order they run.
const (
	StepParse      = "parse"
	StepBinding    = "binding"
	StepChainID    = "chain-id"
	StepEntryPath  = "entry-path"
	StepTxPath     = "tx-path"
	StepBlockHash  = "block-hash"
	StepChainWalk  = "chain-walk"
	StepDigestTime = "digest-time"
)

// Proof is a parsed inclusion proof.
type Proof struct {
	ChainID    [32]byte
	Relation   string // informational
	TableID    uint32
	LedgerTxID uint64
	Seq        uint32
	Op         byte
	RowHash    merkle.Hash
	EntryIndex uint64
	EntryCount uint64
	EntryPath  []merkle.Hash
	TxRoot     merkle.Hash
	UserName   string
	TxIndex    uint64
	TxPath     []merkle.Hash
	Block      format.Header
	// Chain is the headers after the block, when the proof was parsed by Parse.
	// A proof read by ParseStream holds none: only ChainLen, the number of them.
	Chain    []format.Header
	ChainLen uint64
}

// Error is a failed verification step.
type Error struct {
	Step string
	Err  error
}

func (e *Error) Error() string { return e.Step + ": " + e.Err.Error() }

// Unwrap returns the underlying error.
func (e *Error) Unwrap() error { return e.Err }

func fail(step, format string, args ...any) *Error {
	return &Error{Step: step, Err: fmt.Errorf(format, args...)}
}

// Parse parses a proof file with strict integer handling. README: "hex is
// lower case; numbers are JSON integers: a number with a fractional part, or
// outside the range, is rejected". The fields format, chain_id, table_id,
// ledger_txid, seq, op, row_hash, entry_index, entry_count, entry_path,
// tx_root, user_name, tx_index, tx_path, block and chain are required;
// relation is informational and optional. Unknown members are ignored.
func Parse(data []byte) (*Proof, error) {
	// The chain of headers is the one member that grows with the proof: it is
	// read element by element, so the memory of the parse is the headers kept
	// (about 100 bytes each, one per 230 bytes or more of input) and never a tree
	// of the whole file.
	var chainHeaders []format.Header
	v, err := jsonx.ParseStreaming(data, jsonx.DefaultLimits, "chain", func(el jsonx.Value) error {
		h, err := chain.ParseHeader(el)
		chainHeaders = append(chainHeaders, h)
		return err
	})
	if err != nil {
		return nil, err
	}
	p, err := fromValue(v)
	if err != nil {
		return nil, err
	}
	p.Chain = chainHeaders
	p.ChainLen = uint64(len(chainHeaders))
	return p, nil
}

// fromValue reads the members of a parsed proof other than the elements of
// chain.
func fromValue(v jsonx.Value) (*Proof, error) {
	o, err := v.Object()
	if err != nil {
		return nil, err
	}
	var p Proof
	if _, err := o.Int64("format", 1, 1); err != nil {
		return nil, err
	}
	if p.ChainID, err = chain.HashField(o, "chain_id"); err != nil {
		return nil, err
	}
	if o.Has("relation") {
		if p.Relation, err = o.String("relation"); err != nil {
			return nil, err
		}
	}
	n, err := o.Int64("table_id", 0, chain.MaxInt4)
	if err != nil {
		return nil, err
	}
	p.TableID = uint32(n) // #nosec G115 -- range checked
	n, err = o.Int64("ledger_txid", 0, chain.MaxInt8)
	if err != nil {
		return nil, err
	}
	p.LedgerTxID = uint64(n) // #nosec G115 -- range checked
	n, err = o.Int64("seq", 0, chain.MaxInt4)
	if err != nil {
		return nil, err
	}
	p.Seq = uint32(n) // #nosec G115 -- range checked
	op, err := o.String("op")
	if err != nil {
		return nil, err
	}
	switch op {
	case "I":
		p.Op = format.OpInsert
	case "D":
		p.Op = format.OpDelete
	default:
		return nil, fmt.Errorf("%s.op: is %q, expected \"I\" or \"D\"", o.Path(), op)
	}
	if p.RowHash, err = chain.HashField(o, "row_hash"); err != nil {
		return nil, err
	}
	if p.EntryIndex, p.EntryCount, err = indexAndCount(o, "entry_index", "entry_count"); err != nil {
		return nil, err
	}
	if p.EntryPath, err = hashArray(o, "entry_path"); err != nil {
		return nil, err
	}
	if p.TxRoot, err = chain.HashField(o, "tx_root"); err != nil {
		return nil, err
	}
	if p.UserName, err = o.String("user_name"); err != nil {
		return nil, err
	}
	n, err = o.Int64("tx_index", 0, chain.MaxInt4)
	if err != nil {
		return nil, err
	}
	p.TxIndex = uint64(n) // #nosec G115 -- range checked
	if p.TxPath, err = hashArray(o, "tx_path"); err != nil {
		return nil, err
	}
	bv, err := o.Field("block")
	if err != nil {
		return nil, err
	}
	if p.Block, err = chain.ParseHeader(bv); err != nil {
		return nil, err
	}
	if _, err := o.Array("chain"); err != nil {
		return nil, err
	}
	return &p, nil
}

func indexAndCount(o jsonx.Object, idxName, cntName string) (uint64, uint64, error) {
	i, err := o.Int64(idxName, 0, chain.MaxInt4)
	if err != nil {
		return 0, 0, err
	}
	c, err := o.Int64(cntName, 1, chain.MaxInt4)
	if err != nil {
		return 0, 0, err
	}
	return uint64(i), uint64(c), nil // #nosec G115 -- ranges checked
}

func hashArray(o jsonx.Object, name string) ([]merkle.Hash, error) {
	items, err := o.Array(name)
	if err != nil {
		return nil, err
	}
	if len(items) > merkle.MaxPathLength {
		return nil, fmt.Errorf("%s.%s: has %d hashes, expected at most %d", o.Path(), name, len(items), merkle.MaxPathLength)
	}
	out := make([]merkle.Hash, 0, len(items))
	for _, it := range items {
		s, err := it.String()
		if err != nil {
			return nil, err
		}
		h, err := format.DecodeHash(s)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", it.Path(), err)
		}
		out = append(out, h)
	}
	return out, nil
}

// Expect is what the auditor expects the proof to be for. A nil field is not
// checked. Op is the operation the auditor expects the entry to be: "this
// version was inserted" (format.OpInsert) and "this version was removed"
// (format.OpDelete) are different statements, so a caller that means one of
// them says so; nil accepts either, and the result reports which one the
// proof is for.
type Expect struct {
	TableID *uint32
	RowHash *merkle.Hash
	Op      *byte
}

// OpName is the name of an operation byte of an entry: "I" or "D".
func OpName(op byte) string {
	switch op {
	case format.OpInsert:
		return "I"
	case format.OpDelete:
		return "D"
	}
	return fmt.Sprintf("0x%02x", op)
}

// Result describes a successful verification.
type Result struct {
	BlockHeight  uint64
	DigestHeight uint64
	// Steps lists the checks that passed, in order, each with a short detail.
	Steps []Step
}

// Step is one passed check.
type Step struct {
	Name   string
	Detail string
}

// Verify runs the verification steps against the trusted digest d. On
// failure the error is an *Error naming the step.
func Verify(p *Proof, d chain.Digest, exp Expect) (*Result, error) {
	return verifyWith(p, d, exp, func() (format.Header, error) { return reach(p, d) })
}

// verifyWith is Verify with the walk along the chain of headers left to the
// caller: in memory (reach), or read again from the file (reachStream).
func verifyWith(p *Proof, d chain.Digest, exp Expect, walk func() (format.Header, error)) (*Result, error) {
	res := &Result{BlockHeight: p.Block.Height, DigestHeight: d.Height}
	pass := func(name, detail string) { res.Steps = append(res.Steps, Step{name, detail}) }
	if err := verifyRow(p, d.ChainID, "the digest's", "proof and digest name the same chain", exp, pass); err != nil {
		return nil, err
	}

	// Step 4: reach the digest.
	cur, err := walk()
	if err != nil {
		return nil, err
	}
	pass(StepChainWalk, fmt.Sprintf("block %d reaches the digest at height %d through %d later header(s)", p.Block.Height, d.Height, d.Height-p.Block.Height))

	// Informational cross-check of the digest's block_closed_at, when the
	// digest carries it.
	if d.HasClosedAt && d.ClosedAtUS != cur.ClosedAtUS {
		return nil, fail(StepDigestTime, "digest block_closed_at is %s, the block header says %s", chain.FormatUS(d.ClosedAtUS), chain.FormatUS(cur.ClosedAtUS))
	}
	if d.HasClosedAt {
		pass(StepDigestTime, "digest block_closed_at matches the block time")
	}
	return res, nil
}

// VerifyRow runs the steps of a proof that need no digest and no later header: the binding to
// the expected table, row and operation, the chain id, the entry path to the transaction root,
// the transaction path to the block's transaction root, and the block hash recomputed from
// its header. The chain id is the one of the log the caller verifies the block against. It
// proves nothing about where the block is: that is the work of a log leg (an inclusion proof
// of the block header in a signed log).
func VerifyRow(p *Proof, chainID [32]byte, exp Expect) (*Result, error) {
	res := &Result{BlockHeight: p.Block.Height}
	pass := func(name, detail string) { res.Steps = append(res.Steps, Step{name, detail}) }
	if err := verifyRow(p, chainID, "the log's", "proof and log name the same chain", exp, pass); err != nil {
		return nil, err
	}
	return res, nil
}

// verifyRow is steps 0 to 3 of the README: everything about the row and its block that does
// not depend on how the block is tied to a trusted anchor.
func verifyRow(p *Proof, chainID [32]byte, who, same string, exp Expect, pass func(name, detail string)) error {
	// Step 0 of the README: the proof is for the table and row the auditor
	// means, and for the chain of the digest.
	if exp.TableID != nil {
		if p.TableID != *exp.TableID {
			return fail(StepBinding, "proof is for table_id %d, expected %d", p.TableID, *exp.TableID)
		}
	}
	if exp.RowHash != nil {
		if p.RowHash != *exp.RowHash {
			return fail(StepBinding, "proof row_hash is %x, expected %x", p.RowHash, *exp.RowHash)
		}
	}
	if exp.Op != nil {
		if p.Op != *exp.Op {
			return fail(StepBinding, "proof is for an entry with operation %s, expected %s", OpName(p.Op), OpName(*exp.Op))
		}
	}
	if exp.TableID != nil || exp.RowHash != nil || exp.Op != nil {
		pass(StepBinding, "proof is for the expected table, row and operation")
	}
	if p.ChainID != chainID {
		return fail(StepChainID, "proof chain_id is %x, %s is %x", p.ChainID, who, chainID)
	}
	pass(StepChainID, same)

	// Step 1: the entry leaf and its path to tx_root.
	e := format.EntryLeafHash(p.Seq, p.TableID, p.Op, p.RowHash)
	if err := merkle.VerifyPath(e, p.EntryIndex, p.EntryCount, p.EntryPath, p.TxRoot); err != nil {
		return &Error{Step: StepEntryPath, Err: err}
	}
	pass(StepEntryPath, fmt.Sprintf("entry %d of %d leads to the transaction root", p.EntryIndex, p.EntryCount))

	// Step 2: the transaction leaf and its path to the block's tx_merkle_root.
	if p.EntryCount > math.MaxUint32 {
		return fail(StepTxPath, "entry_count %d does not fit 32 bits", p.EntryCount)
	}
	t, err := format.BlockTxLeafHash(p.LedgerTxID, uint32(p.EntryCount), p.TxRoot, p.UserName)
	if err != nil {
		return &Error{Step: StepTxPath, Err: err}
	}
	if err := merkle.VerifyPath(t, p.TxIndex, uint64(p.Block.TxCount), p.TxPath, p.Block.TxMerkleRoot); err != nil {
		return &Error{Step: StepTxPath, Err: err}
	}
	pass(StepTxPath, fmt.Sprintf("transaction %d of %d leads to the block's transaction root", p.TxIndex, p.Block.TxCount))

	// Step 3: the block hash from the header fields.
	if err := chain.CheckHeader(p.Block); err != nil {
		return &Error{Step: StepBlockHash, Err: err}
	}
	if p.Block.Height == 0 {
		if want := format.GenesisHash(chainID); p.Block.PrevHash != want {
			return fail(StepBlockHash, "block 0 prev_hash is %x, expected %x (SHA-256 of 0x14 and the chain id)", p.Block.PrevHash, want)
		}
	}
	pass(StepBlockHash, fmt.Sprintf("block %d hash recomputed", p.Block.Height))

	return nil
}

// reach walks from the proof's block to the digest height and returns the
// header at that height, whose hash equals the digest's.
func reach(p *Proof, d chain.Digest) (format.Header, error) {
	if d.Height < p.Block.Height {
		return format.Header{}, fail(StepChainWalk, "digest is at height %d, older than the proof's block %d: it cannot prove anything about this block", d.Height, p.Block.Height)
	}
	cur := p.Block
	need := d.Height - p.Block.Height
	if uint64(len(p.Chain)) < need {
		return format.Header{}, fail(StepChainWalk, "chain has %d header(s), expected at least %d to reach height %d", len(p.Chain), need, d.Height)
	}
	for i := uint64(0); i < need; i++ {
		next := p.Chain[i]
		if next.HashVersion != format.HashVersion {
			return format.Header{}, fail(StepChainWalk, "chain[%d]: hash_version is %d, expected %d", i, next.HashVersion, format.HashVersion)
		}
		if err := chain.CheckLink(cur, next); err != nil {
			return format.Header{}, &Error{Step: StepChainWalk, Err: fmt.Errorf("chain[%d]: %w", i, err)}
		}
		if err := chain.CheckHeader(next); err != nil {
			return format.Header{}, &Error{Step: StepChainWalk, Err: fmt.Errorf("chain[%d]: %w", i, err)}
		}
		cur = next
	}
	if cur.BlockHash != d.BlockHash {
		return format.Header{}, fail(StepChainWalk, "block at height %d has hash %x, the digest says %x", d.Height, cur.BlockHash, d.BlockHash)
	}
	return cur, nil
}

// IsStep reports whether err is a failed verification step with this name.
func IsStep(err error, step string) bool {
	var e *Error
	return errors.As(err, &e) && e.Step == step
}
