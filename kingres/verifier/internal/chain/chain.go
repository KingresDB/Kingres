// SPDX-License-Identifier: AGPL-3.0-only

// Package chain parses and checks block headers, digests and the block hash
// chain of a KingresDB ledger.
package chain

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"math"
	"time"

	"github.com/KingresDB/Kingres/kingres/verifier/internal/format"
	"github.com/KingresDB/Kingres/kingres/verifier/internal/jsonx"
	"github.com/KingresDB/Kingres/kingres/verifier/internal/merkle"
)

// Integer ranges of the database types behind the JSON numbers: "int" is
// int4, "bigint" is int8.
const (
	// MaxInt4 is the largest value of a PostgreSQL int.
	MaxInt4 = math.MaxInt32
	// MaxInt8 is the largest value of a PostgreSQL bigint.
	MaxInt8 = math.MaxInt64
)

// pgEpochUnix is 2000-01-01 00:00:00 UTC in Unix seconds: the origin of
// closed_at ("microseconds since 2000-01-01 00:00:00 UTC").
const pgEpochUnix = 946684800

// Digest is the part of a digest that verifiers use, plus the informational
// fields.
type Digest struct {
	Database    string
	ChainID     [32]byte
	HashVersion int64
	Height      uint64
	BlockHash   merkle.Hash
	// ClosedAtUS is block_closed_at in microseconds since 2000-01-01 UTC;
	// HasClosedAt is false when the field is absent.
	ClosedAtUS  int64
	HasClosedAt bool
	// GeneratedAtUS is generated_at in the same unit; HasGeneratedAt is false
	// when the field is absent.
	GeneratedAtUS  int64
	HasGeneratedAt bool
}

// ParseDigest parses a digest file. README: "A JSON object; chain_id, height
// and block_hash (lower case hex of the bytes) are what verifiers use".
// chain_id, hash_version, height and block_hash are required; database,
// block_closed_at and generated_at are informational and checked only when
// present. Unknown members are ignored.
func ParseDigest(data []byte) (Digest, error) {
	v, err := jsonx.Parse(data)
	if err != nil {
		return Digest{}, fmt.Errorf("digest: %w", err)
	}
	o, err := v.Object()
	if err != nil {
		return Digest{}, fmt.Errorf("digest: %w", err)
	}
	var d Digest
	if d.ChainID, err = HashField(o, "chain_id"); err != nil {
		return Digest{}, fmt.Errorf("digest: %w", err)
	}
	if d.HashVersion, err = o.Int64("hash_version", 1, 1); err != nil {
		return Digest{}, fmt.Errorf("digest: %w", err)
	}
	h, err := o.Int64("height", 0, MaxInt8)
	if err != nil {
		return Digest{}, fmt.Errorf("digest: %w", err)
	}
	d.Height = uint64(h) // #nosec G115 -- range checked
	if d.BlockHash, err = HashField(o, "block_hash"); err != nil {
		return Digest{}, fmt.Errorf("digest: %w", err)
	}
	if o.Has("database") {
		if d.Database, err = o.String("database"); err != nil {
			return Digest{}, fmt.Errorf("digest: %w", err)
		}
	}
	if o.Has("block_closed_at") {
		if d.ClosedAtUS, err = timeField(o, "block_closed_at"); err != nil {
			return Digest{}, fmt.Errorf("digest: %w", err)
		}
		d.HasClosedAt = true
	}
	if o.Has("generated_at") {
		if d.GeneratedAtUS, err = timeField(o, "generated_at"); err != nil {
			return Digest{}, fmt.Errorf("digest: %w", err)
		}
		d.HasGeneratedAt = true
	}
	return d, nil
}

func timeField(o jsonx.Object, name string) (int64, error) {
	s, err := o.String(name)
	if err != nil {
		return 0, err
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return 0, fmt.Errorf("%s.%s: %q is not an RFC 3339 timestamp, expected for example 2026-11-14T10:20:30.123456Z", o.Path(), name, s)
	}
	return t.Unix()*1_000_000 + int64(t.Nanosecond()/1000) - pgEpochUnix*1_000_000, nil
}

// HashField reads a 32-byte lower case hex string member.
func HashField(o jsonx.Object, name string) ([32]byte, error) {
	s, err := o.String(name)
	if err != nil {
		return [32]byte{}, err
	}
	h, err := format.DecodeHash(s)
	if err != nil {
		return [32]byte{}, fmt.Errorf("%s.%s: %w", o.Path(), name, err)
	}
	return h, nil
}

// ParseHeader reads a block header object: height, hash_version, prev_hash,
// tx_merkle_root, tx_count, closed_at_us and block_hash. It checks only the
// types and ranges; use CheckHeader for the hash.
func ParseHeader(v jsonx.Value) (format.Header, error) {
	o, err := v.Object()
	if err != nil {
		return format.Header{}, err
	}
	var h format.Header
	height, err := o.Int64("height", 0, MaxInt8)
	if err != nil {
		return h, err
	}
	h.Height = uint64(height) // #nosec G115 -- range checked
	hv, err := o.Int64("hash_version", 1, 1)
	if err != nil {
		return h, err
	}
	h.HashVersion = uint16(hv) // #nosec G115 -- range checked
	if h.PrevHash, err = HashField(o, "prev_hash"); err != nil {
		return h, err
	}
	if h.TxMerkleRoot, err = HashField(o, "tx_merkle_root"); err != nil {
		return h, err
	}
	n, err := o.Int64("tx_count", 1, MaxInt4)
	if err != nil {
		return h, err
	}
	h.TxCount = uint32(n) // #nosec G115 -- range checked
	if h.ClosedAtUS, err = o.Int64("closed_at_us", math.MinInt64, math.MaxInt64); err != nil {
		return h, err
	}
	if h.BlockHash, err = HashField(o, "block_hash"); err != nil {
		return h, err
	}
	return h, nil
}

// CheckHeader checks that the block hash of h equals the hash recomputed from
// its other fields.
func CheckHeader(h format.Header) error {
	if got := h.ComputeHash(); got != h.BlockHash {
		return fmt.Errorf("block %d: block_hash is %x, expected %x recomputed from its header fields", h.Height, h.BlockHash, got)
	}
	return nil
}

// CheckLink checks that next follows prev (whose hash must already have been
// checked): the next height, prev_hash equal to the hash of prev, and a
// closed_at that is not earlier.
func CheckLink(prev, next format.Header) error {
	if next.Height != prev.Height+1 {
		return fmt.Errorf("block after %d has height %d, expected %d", prev.Height, next.Height, prev.Height+1)
	}
	if next.PrevHash != prev.BlockHash {
		return fmt.Errorf("block %d: prev_hash is %x, expected %x (the hash of block %d)", next.Height, next.PrevHash, prev.BlockHash, prev.Height)
	}
	if next.ClosedAtUS < prev.ClosedAtUS {
		return fmt.Errorf("block %d: closed_at_us %d is earlier than %d of block %d, expected block times that never go back", next.Height, next.ClosedAtUS, prev.ClosedAtUS, prev.Height)
	}
	return nil
}

// ParseBlocks parses a block header export: one JSON object per line. Blank
// lines are rejected, except that the file may end with a newline.
func ParseBlocks(data []byte) ([]format.Header, error) {
	if len(data) == 0 {
		return nil, errors.New("blocks file is empty, expected one JSON object per line starting with height 0")
	}
	lines := bytes.Split(bytes.TrimSuffix(data, []byte("\n")), []byte("\n"))
	out := make([]format.Header, 0, len(lines))
	for i, line := range lines {
		line = bytes.TrimSuffix(line, []byte("\r"))
		v, err := jsonx.Parse(line)
		if err != nil {
			return nil, fmt.Errorf("blocks line %d: %w", i+1, err)
		}
		h, err := ParseHeader(v)
		if err != nil {
			return nil, fmt.Errorf("blocks line %d: %w", i+1, err)
		}
		out = append(out, h)
	}
	return out, nil
}

// VerifyBlocks recomputes the chain from the genesis hash of chainID: the
// first header must be block 0 with the genesis prev_hash, every block hash
// must be recomputed correctly, heights must be dense and block times must
// not go back.
func VerifyBlocks(headers []format.Header, chainID [32]byte) error {
	if len(headers) == 0 {
		return errors.New("no blocks, expected at least block 0")
	}
	first := headers[0]
	if first.Height != 0 {
		return fmt.Errorf("first block has height %d, expected 0", first.Height)
	}
	if want := format.GenesisHash(chainID); first.PrevHash != want {
		return fmt.Errorf("block 0: prev_hash is %x, expected %x (SHA-256 of 0x14 and the chain id)", first.PrevHash, want)
	}
	for i, h := range headers {
		if err := CheckHeader(h); err != nil {
			return err
		}
		if i > 0 {
			if err := CheckLink(headers[i-1], h); err != nil {
				return err
			}
		}
	}
	return nil
}

// StreamError is a failure of StreamBlocks with the step it belongs to
// (parse, chain or digest), the steps the blocks command reports.
type StreamError struct {
	Step string
	Err  error
}

func (e *StreamError) Error() string { return e.Err.Error() }

// Unwrap returns the cause.
func (e *StreamError) Unwrap() error { return e.Err }

// maxBlockLine bounds one line of an export: a header is about 300 bytes.
const maxBlockLine = 1 << 20

// StreamBlocks verifies an export line by line and keeps one header at a time,
// so that its memory does not grow with the size of the file (a chain of
// millions of blocks is hundreds of megabytes of input). It checks what
// ParseBlocks and VerifyBlocks check, and, when d is not nil, that the chain
// reaches the digest (CheckDigestOnBlocks). It returns the number of blocks.
// The first problem found is reported, in file order.
func StreamBlocks(r io.Reader, chainID [32]byte, d *Digest) (int, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), maxBlockLine)
	var prev format.Header
	n := 0
	reached := d == nil
	for sc.Scan() {
		line := sc.Bytes()
		v, err := jsonx.Parse(line)
		var h format.Header
		if err == nil {
			h, err = ParseHeader(v)
		}
		if err != nil {
			return n, &StreamError{"parse", fmt.Errorf("blocks line %d: %w", n+1, err)}
		}
		if n == 0 {
			if h.Height != 0 {
				return n, &StreamError{"chain", fmt.Errorf("first block has height %d, expected 0", h.Height)}
			}
			if want := format.GenesisHash(chainID); h.PrevHash != want {
				return n, &StreamError{"chain", fmt.Errorf("block 0: prev_hash is %x, expected %x (SHA-256 of 0x14 and the chain id)", h.PrevHash, want)}
			}
		}
		if err := CheckHeader(h); err != nil {
			return n, &StreamError{"chain", err}
		}
		if n > 0 {
			if err := CheckLink(prev, h); err != nil {
				return n, &StreamError{"chain", err}
			}
		}
		if d != nil && h.Height == d.Height {
			if err := checkDigestHeader(h, *d); err != nil {
				return n + 1, &StreamError{"digest", err}
			}
			reached = true
		}
		prev = h
		n++
	}
	if err := sc.Err(); err != nil {
		if errors.Is(err, bufio.ErrTooLong) {
			err = fmt.Errorf("a line is longer than %d bytes", maxBlockLine)
		}
		return n, &StreamError{"parse", fmt.Errorf("blocks line %d: %w", n+1, err)}
	}
	if n == 0 {
		return 0, &StreamError{"parse", errors.New("blocks file is empty, expected one JSON object per line starting with height 0")}
	}
	if !reached {
		return n, &StreamError{"digest", fmt.Errorf("the blocks end at height %d, expected a block at the digest height %d", prev.Height, d.Height)}
	}
	return n, nil
}

// CheckDigestOnBlocks checks that the (already verified) headers reach the
// digest: a block at the digest height exists, with the digest's block hash
// and, when the digest carries it, its block time.
func CheckDigestOnBlocks(headers []format.Header, d Digest) error {
	if len(headers) == 0 {
		return errors.New("no blocks, expected a block at the digest height")
	}
	if last := headers[len(headers)-1].Height; last < d.Height {
		return fmt.Errorf("the blocks end at height %d, expected a block at the digest height %d", last, d.Height)
	}
	return checkDigestHeader(headers[d.Height], d)
}

// checkDigestHeader checks that h, the block at the digest height, is the
// block the digest names.
func checkDigestHeader(h format.Header, d Digest) error {
	if h.Height != d.Height {
		return fmt.Errorf("block at position %d has height %d, expected dense heights from 0", d.Height, h.Height)
	}
	if h.BlockHash != d.BlockHash {
		return fmt.Errorf("block %d has hash %x, the digest says %x", d.Height, h.BlockHash, d.BlockHash)
	}
	if d.HasClosedAt && d.ClosedAtUS != h.ClosedAtUS {
		return fmt.Errorf("block %d closed_at_us is %d, the digest's block_closed_at is %d microseconds since 2000-01-01", d.Height, h.ClosedAtUS, d.ClosedAtUS)
	}
	return nil
}

// FormatUS renders microseconds since 2000-01-01 UTC as an RFC 3339
// timestamp, or as a plain number when it is outside the years 1 to 9999.
func FormatUS(us int64) string {
	const lim = 250_000_000_000 * 1_000_000
	if us < -lim || us > lim {
		return fmt.Sprintf("%d us since 2000-01-01", us)
	}
	secs := us / 1_000_000
	rem := us % 1_000_000
	if rem < 0 {
		rem += 1_000_000
		secs--
	}
	return time.Unix(pgEpochUnix+secs, rem*1000).UTC().Format("2006-01-02T15:04:05.000000Z07:00")
}
