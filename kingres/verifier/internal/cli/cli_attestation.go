// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package cli

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/KingresDB/Kingres/kingres/verifier/internal/attest"
	"github.com/KingresDB/Kingres/kingres/verifier/internal/chain"
	"github.com/KingresDB/Kingres/kingres/verifier/internal/format"
	"github.com/KingresDB/Kingres/kingres/verifier/internal/proof"
	"github.com/KingresDB/Kingres/kingres/verifier/internal/rowfile"
	"github.com/KingresDB/Kingres/kingres/verifier/internal/safefile"
)

// noRevocationShown is the wording the design fixes (7.4, "What attestation does"): an inclusion proof
// shows presence, not absence, so the verifier never says that no revocation exists.
const noRevocationShown = "no revocation shown in the evidence given"

const noRevocationCaveat = "that no revocation of the key exists: an inclusion proof shows presence, not absence. The completeness of the list of revocations is a claim of the server, unless you read the whole table kingres.ledger_client_key_revocation through ledger_verify on a copy"

// anchorer ties a proof to something the auditor trusts: a digest (the linear walk of the README), or a
// signed log (the log leg).
type anchorer struct {
	digest *chain.Digest
	p      *pubInputs
	leg    string
	aggID  string
	epoch  uint64
}

// verify verifies one proof and adds its steps to r under the label. It returns the proof.
func (e *env) verifyAnchored(r *result, label string, an *anchorer, open proof.Source, exp proof.Expect) (*proof.Proof, outcome) {
	sub := &result{cmd: r.cmd}
	var pr proof.Proof
	var out outcome
	if an.digest != nil {
		p, sum, err := proof.ParseStream(open)
		if err != nil {
			return nil, failedOut(label+": parse", err)
		}
		res, err := proof.VerifyStream(p, sum, *an.digest, exp, open)
		if res != nil {
			for _, s := range res.Steps {
				sub.ok(s.Name, s.Detail)
			}
		}
		if err != nil {
			var pe *proof.Error
			if errors.As(err, &pe) {
				return nil, failedOut(label+": "+pe.Step, pe.Err)
			}
			return nil, failedOut(label, err)
		}
		pr = *p
	} else {
		out = e.proofLegCore(sub, an.p, open, exp, proofLegArgs{leg: an.leg, aggID: an.aggID, epoch: an.epoch}, &pr)
		if !out.ok() {
			if out.kind == outFailed {
				out.step = label + ": " + out.step
			}
			if out.kind == outIncomplete {
				for i := range out.missing {
					out.missing[i] = label + ": " + out.missing[i]
				}
			}
			return nil, out
		}
	}
	for _, s := range sub.steps {
		r.ok(label+" "+s.Name, s.Detail)
	}
	return &pr, outcome{}
}

type revEvidence struct {
	name string
	open proof.Source
	cols []byte
}

type attestInput struct {
	an         *anchorer
	rowOpen    proof.Source
	attOpen    proof.Source
	attCols    []byte
	attTableID uint32
	clientKey  string // hex key id the auditor expects, or ""
	trust      *pubInputs
	revs       []revEvidence
	revTableID *uint32
	exp        proof.Expect
}

// attestCore is the check of one client attestation (design 7.4, "What attestation does"; correction 160
// of this verifier says where it reads the design as it is silent).
func (e *env) attestCore(r *result, in attestInput) outcome {
	row, out := e.verifyAnchored(r, "row", in.an, in.rowOpen, in.exp)
	if !out.ok() {
		return out
	}
	// (2) the attestation row: its row hash from the columns, its proof, for the attestation table, of the same chain
	cols, err := rowfile.Parse(in.attCols)
	if err != nil {
		return failedOut("attestation-columns", err)
	}
	att, err := attest.ParseAttestation(cols)
	if err != nil {
		return failedOut("attestation-columns", err)
	}
	ah, err := format.RowHash(in.attTableID, cols)
	if err != nil {
		return failedOut("attestation-columns", err)
	}
	tid := in.attTableID
	apr, out := e.verifyAnchored(r, "attestation", in.an, in.attOpen, proof.Expect{TableID: &tid, RowHash: &ah})
	if !out.ok() {
		return out
	}
	if apr.ChainID != row.ChainID {
		return failedOut("attestation-chain", fmt.Errorf("the attestation row is in the chain %x and the row in the chain %x: an attestation belongs to the chain of its entry", apr.ChainID, row.ChainID))
	}
	// the attestation row is itself an entry of the chain: an insert, in the transaction of the entry it attests (ledger_attest runs
	// in that transaction), so a row that was deleted, or that sits in another transaction, is not an attestation of this entry
	if apr.Op != format.OpInsert {
		return failedOut("attestation-row", fmt.Errorf("the entry of the attestation row is an operation %s: an attestation is an insert into the attestation table", proof.OpName(apr.Op)))
	}
	if apr.LedgerTxID != row.LedgerTxID {
		return failedOut("attestation-row", fmt.Errorf("the entry of the attestation row is in transaction %d and the entry it attests in transaction %d: ledger_attest runs in the transaction of the entry", apr.LedgerTxID, row.LedgerTxID))
	}
	// (3) the attestation names the entry of the row proof
	if att.LedgerTxID != row.LedgerTxID || att.Seq != row.Seq {
		return failedOut("attestation-entry", fmt.Errorf("the attestation row names transaction %d, entry %d; the proof of the row is for transaction %d, entry %d", att.LedgerTxID, att.Seq, row.LedgerTxID, row.Seq))
	}
	if apr.Block.Height != row.Block.Height || apr.Block.BlockHash != row.Block.BlockHash {
		return failedOut("attestation-entry", fmt.Errorf("the attestation row is in block %d and the entry it attests in block %d: ledger_attest runs in the transaction of the entry, so both are in one block", apr.Block.Height, row.Block.Height))
	}
	r.ok("attestation-entry", fmt.Sprintf("the attestation row names transaction %d, entry %d, the entry of the row proof, in block %d", att.LedgerTxID, att.Seq, row.Block.Height))
	// (4) the key is pinned, and the signature verifies
	kid := hex.EncodeToString(att.KeyID[:])
	if in.clientKey != "" && in.clientKey != kid {
		return failedOut("client-key", fmt.Errorf("the attestation is signed under key %s, and you expected %s", kid, in.clientKey))
	}
	var pub []byte
	for _, k := range in.trust.trust.ClientKeys {
		if k.KeyID == kid {
			pub = k.Public
		}
	}
	if pub == nil {
		return failedOut("client-key", fmt.Errorf("the key %s is not pinned in the trust file (client_keys): a key in the registry of the server is not trusted, a row signed by a key you did not pin fails", kid))
	}
	msg := attest.Message(row.ChainID, row.TableID, row.Op, row.RowHash, att.KeyID, att.Nonce)
	if err := attest.Verify(pub, msg, att.Signature[:]); err != nil {
		return failedOut("signature", fmt.Errorf("the attestation by key %s for chain %x, table %d, operation %s, row hash %x: %w", kid, row.ChainID[:8], row.TableID, proof.OpName(row.Op), row.RowHash[:8], err))
	}
	label := ""
	for _, k := range in.trust.trust.ClientKeys {
		if k.KeyID == kid {
			label = k.Label
		}
	}
	r.ok("signature", fmt.Sprintf("the Ed25519 signature over the 136-byte message verifies under the pinned key %s (%q)", kid, label))
	// (5) revocations
	revoked := 0
	for _, rv := range in.revs {
		rpr, out := e.verifyAnchored(r, "revocation "+rv.name, in.an, rv.open, proof.Expect{TableID: in.revTableID})
		if !out.ok() {
			return out
		}
		rc, err := rowfile.Parse(rv.cols)
		if err != nil {
			return failedOut("revocation-columns", fmt.Errorf("%s: %w", rv.name, err))
		}
		rev, err := attest.ParseRevocation(rc)
		if err != nil {
			return failedOut("revocation-columns", fmt.Errorf("%s: %w", rv.name, err))
		}
		rh, err := format.RowHash(*in.revTableID, rc)
		if err != nil || rh != rpr.RowHash {
			return failedOut("revocation-columns", fmt.Errorf("%s: the columns file does not hash to the row hash %x of its proof", rv.name, rpr.RowHash[:8]))
		}
		if rpr.ChainID != row.ChainID || rpr.Op != format.OpInsert {
			return failedOut("revocation", fmt.Errorf("%s: a revocation is an insert in the chain of the entry", rv.name))
		}
		if rev.KeyID != att.KeyID {
			r.notes = append(r.notes, fmt.Sprintf("the revocation %s is of another key (%x), and does not matter here", rv.name, rev.KeyID[:]))
			continue
		}
		if attest.RevokedBefore(row.Block.Height, rpr.Block.Height) {
			return failedOut("revoked", fmt.Errorf("key %s was revoked in block %d (%q), at or before block %d of the attestation: an attestation is valid only when its block is lower than the block of the revocation (the same block counts as after, because the order inside a block is by transaction id, not by commit order)", kid, rpr.Block.Height, rev.Reason, row.Block.Height))
		}
		revoked++
		r.notes = append(r.notes, fmt.Sprintf("key %s was revoked in block %d, after block %d of the attestation: the attestation made before the revocation stays valid", kid, rpr.Block.Height, row.Block.Height))
	}
	if len(in.revs) == 0 {
		r.notes = append(r.notes, noRevocationShown+": no revocation evidence was supplied (--revocations), so none was looked at")
	} else {
		r.notes = append(r.notes, fmt.Sprintf("%s: %d revocation(s) of this key and %d row(s) of revocations were examined", noRevocationShown, revoked, len(in.revs)))
	}
	r.notProved = append(r.notProved, noRevocationCaveat,
		"that the signer saw more than the hash: a client that signs the hash the server returned proves only that it approved the server's claim; recompute the row hash from your own data (row-hash)",
		"that the holder of the key is the person the label names: the mapping from key to person is the registry's label, chosen by an administrator, and the label in your trust file is yours",
		"that rows without an attestation were not written by someone else: this checks one entry",
		"that the table ids given are those of the tables kingres.ledger_attestation and kingres.ledger_client_key_revocation: they come from the server's registry, and a signature under a key you pinned is what a row of another table cannot have")
	r.info = map[string]any{"chain_id": hex.EncodeToString(row.ChainID[:]), "table_id": row.TableID, "op": proof.OpName(row.Op), "row_hash": hex.EncodeToString(row.RowHash[:]),
		"block_height": row.Block.Height, "key_id": kid, "key_label": label, "revocations_examined": len(in.revs), "revocation_shown": false}
	r.headline = fmt.Sprintf("the entry (table %d, operation %s, row hash %x) in block %d carries a valid signature of the pinned key %s (%s).", row.TableID, proof.OpName(row.Op), row.RowHash[:8], row.Block.Height, kid, noRevocationShown)
	return outcome{}
}

func (e *env) runAttestation(args []string) int {
	fs, asJSON := e.newFlags("attestation", "--trust FILE --proof ROW.json --attestation ROW.ATT.json --attestation-columns FILE --attestation-table-id N (--digest FILE | --bundle SOURCE --log-leg chain|aggregate) [--client-key HEX] [--revocations DIR --revocation-table-id N] [--table-id N --row-hash HEX --op I|D|any] [--json]",
		"Check a client attestation: the proof of the row's entry, the proof of the attestation row of\n"+
			"kingres.ledger_attestation (its row hash is recomputed from the columns file, its table id is the one you give)\n"+
			"against the same anchor (a digest, or a signed log), that the attestation row names the entry of the row\n"+
			"proof, and that the Ed25519 signature over the 136-byte message (chain id, table id, operation, row hash,\n"+
			"key id, nonce) verifies under a key PINNED in client_keys of the trust file (a key that is only in the\n"+
			"server's registry fails). With --revocations (a directory of NAME.proof.json and NAME.columns.json for rows of\n"+
			"kingres.ledger_client_key_revocation) a revocation of the key in a block at or before the attestation's block\n"+
			"fails. The verifier cannot show that no revocation exists: it says \""+noRevocationShown+"\".")
	trustPath := fs.String("trust", "", "trust file with client_keys (JSON)")
	proofPath := fs.String("proof", "", "proof of the row's entry")
	attPath := fs.String("attestation", "", "proof of the attestation row")
	attCols := fs.String("attestation-columns", "", "columns file of the attestation row (ledger_txid, seq, key_id, nonce, signature)")
	attTable := fs.Int64("attestation-table-id", -1, "the table_id of kingres.ledger_attestation (from kingres.ledger_table)")
	digest := fs.String("digest", "", "trusted digest file")
	bundle := fs.String("bundle", "", "the publication or bundle holding the signed logs (directory, .tar.gz or https address)")
	logLeg := fs.String("log-leg", "", "with --bundle: chain or aggregate")
	epoch := fs.Uint64("epoch", 0, "with --bundle: the epoch of the chain log")
	aggID := fs.String("aggregate-id", "", "with --bundle --log-leg aggregate: the aggregate id")
	clientKey := fs.String("client-key", "", "the key id (32 hex characters) you expect the attestation to be signed with")
	revDir := fs.String("revocations", "", "directory of revocation evidence: NAME.proof.json and NAME.columns.json")
	revTable := fs.Int64("revocation-table-id", -1, "the table_id of kingres.ledger_client_key_revocation")
	tableID := fs.Int64("table-id", -1, "expected table id of the row proof")
	rowHash := fs.String("row-hash", "", "expected row hash of the row proof")
	opFlag := fs.String("op", "any", "expected operation of the row's entry: I, D or any")
	nowFlag := fs.String("now", "", "time for key validity windows, RFC 3339 (default: system clock)")
	if code, done := e.parseFlags(fs, args); done {
		return code
	}
	const cmd = "attestation"
	r := &result{cmd: cmd}
	o := e.attestationRun(r, attestArgs{trust: *trustPath, proof: *proofPath, att: *attPath, attCols: *attCols, attTable: *attTable, digest: *digest, bundle: *bundle,
		leg: *logLeg, epoch: *epoch, aggID: *aggID, clientKey: *clientKey, revDir: *revDir, revTable: *revTable, tableID: *tableID, rowHash: *rowHash, op: *opFlag, now: nowOf(fs, nowFlag)})
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

type attestArgs struct {
	trust, proof, att, attCols, digest, bundle, leg, aggID, clientKey, revDir, rowHash, op string
	attTable, revTable, tableID                                                            int64
	epoch                                                                                  uint64
	now                                                                                    nowArg
}

func fileOpener(path string) proof.Source {
	return func() (io.ReadCloser, error) { return safefile.OpenArg(path, maxProofSize) }
}

func (e *env) attestationRun(r *result, a attestArgs) outcome {
	for _, c := range [][2]string{{"proof", a.proof}, {"attestation", a.att}, {"attestation-columns", a.attCols}, {"trust", a.trust}} {
		if c[1] == "" {
			return inputOut(fmt.Errorf("missing --%s", c[0]))
		}
	}
	if a.attTable < 0 || a.attTable > chain.MaxInt4 {
		return inputOut(errors.New("--attestation-table-id is required: the table_id of kingres.ledger_attestation, from kingres.ledger_table (0 to 2147483647)"))
	}
	if (a.digest == "") == (a.bundle == "") {
		return inputOut(errors.New("give exactly one anchor for the proofs: --digest, or --bundle with --log-leg"))
	}
	if a.clientKey != "" && !regexp16.MatchString(a.clientKey) {
		return inputOut(errors.New("--client-key must be 32 lower case hex characters (the key id)"))
	}
	if a.bundle != "" && a.leg != "chain" && a.leg != "aggregate" {
		return inputOut(errors.New("--log-leg is chain or aggregate"))
	}
	exp, err := parseExpect(a.tableID, a.rowHash, a.op)
	if err != nil {
		return inputOut(err)
	}
	p, err := loadPubInputs(a.trust, a.bundle, a.now)
	if err != nil {
		return inputOut(err)
	}
	defer p.close()
	if o := p.gate(); !o.ok() {
		return o
	}
	r.notes = append(r.notes, p.notes...)
	an := &anchorer{p: p, leg: a.leg, aggID: a.aggID, epoch: a.epoch}
	if a.digest != "" {
		data, err := readArg(a.digest, maxDigestFile)
		if err != nil {
			return inputOut(err)
		}
		d, err := chain.ParseDigest(data)
		if err != nil {
			return inputOut(err)
		}
		an.digest = &d
	}
	cols, err := readArg(a.attCols, maxFileSize)
	if err != nil {
		return inputOut(err)
	}
	in := attestInput{an: an, rowOpen: fileOpener(a.proof), attOpen: fileOpener(a.att), attCols: cols, attTableID: uint32(a.attTable), clientKey: a.clientKey, trust: p, exp: exp} // #nosec G115 -- range checked
	for _, f := range []string{a.proof, a.att} {
		pr, err := safefile.OpenArg(f, maxProofSize)
		if err != nil {
			return inputOut(err)
		}
		_ = pr.Close()
	}
	if a.revDir != "" {
		if a.revTable < 0 || a.revTable > chain.MaxInt4 {
			return inputOut(errors.New("--revocation-table-id is required with --revocations: the table_id of kingres.ledger_client_key_revocation"))
		}
		t := uint32(a.revTable) // #nosec G115 -- range checked
		in.revTableID = &t
		dir, err := safefile.OpenDir(a.revDir)
		if err != nil {
			return inputOut(err)
		}
		names, others, err := dir.RegularNames()
		if err == nil && len(others) > 0 {
			err = fmt.Errorf("%s holds an entry that is not a regular file: %s", a.revDir, others[0])
		}
		if err != nil {
			_ = dir.Close()
			return inputOut(err)
		}
		for _, n := range names {
			base, ok := strings.CutSuffix(n, ".proof.json")
			if !ok {
				continue
			}
			colsData, cerr := dir.ReadEntry(base+".columns.json", maxFileSize)
			if cerr != nil {
				_ = dir.Close()
				return inputOut(fmt.Errorf("the revocation %s has no readable %s.columns.json: %w", n, base, cerr))
			}
			pd, perr := dir.ReadEntry(n, maxFileSize)
			if perr != nil {
				_ = dir.Close()
				return inputOut(perr)
			}
			in.revs = append(in.revs, revEvidence{name: base, open: bytesOpener(pd), cols: colsData})
		}
		_ = dir.Close()
	}
	return e.attestCore(r, in)
}

func bytesOpener(b []byte) proof.Source {
	return func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(b)), nil }
}
