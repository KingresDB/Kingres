// SPDX-License-Identifier: AGPL-3.0-only

package ots

import (
	"bytes"
	"fmt"
	"sort"
)

// Attestation tags (8 bytes).
var (
	TagPending = [8]byte{0x83, 0xdf, 0xe3, 0x0d, 0x2e, 0xf9, 0x0c, 0x8e}
	TagBitcoin = [8]byte{0x05, 0x88, 0x96, 0x0d, 0x73, 0xd7, 0x19, 0x01}
)

// AttKind is the kind of an attestation.
type AttKind int

const (
	// AttPending: the commitment is recorded at a calendar (URI).
	AttPending AttKind = iota + 1
	// AttBitcoin: the message is the merkle root of the Bitcoin block at Height.
	AttBitcoin
	// AttUnknown: any other tag (Litecoin, Ethereum, ...), kept opaque.
	AttUnknown
)

// Attestation is a leaf of the tree.
type Attestation struct {
	Kind    AttKind
	URI     string  // AttPending
	Height  uint64  // AttBitcoin
	Tag     [8]byte // all kinds (set by the constructors)
	Payload []byte  // AttUnknown: the raw payload
}

// NewPending returns a pending attestation; the URI is checked.
func NewPending(uri string) (Attestation, error) {
	if err := checkURI([]byte(uri)); err != nil {
		return Attestation{}, err
	}
	return Attestation{Kind: AttPending, URI: uri, Tag: TagPending}, nil
}

// NewBitcoin returns a Bitcoin block header attestation.
func NewBitcoin(height uint64) (Attestation, error) {
	if height > MaxHeight {
		return Attestation{}, fmt.Errorf("ots: block height %d too large", height)
	}
	return Attestation{Kind: AttBitcoin, Height: height, Tag: TagBitcoin}, nil
}

const uriChars = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-._/:"

func checkURI(u []byte) error {
	if len(u) > MaxURILen {
		return fmt.Errorf("%w: longer than %d bytes", ErrURI, MaxURILen)
	}
	for _, c := range u {
		if !bytes.ContainsRune([]byte(uriChars), rune(c)) {
			return fmt.Errorf("%w: character 0x%02x not allowed", ErrURI, c)
		}
	}
	return nil
}

func compareAtt(a, b Attestation) int {
	if c := bytes.Compare(a.Tag[:], b.Tag[:]); c != 0 {
		return c
	}
	switch a.Kind {
	case AttPending:
		switch {
		case a.URI < b.URI:
			return -1
		case a.URI > b.URI:
			return 1
		}
		return 0
	case AttBitcoin:
		switch {
		case a.Height < b.Height:
			return -1
		case a.Height > b.Height:
			return 1
		}
		return 0
	}
	return bytes.Compare(a.Payload, b.Payload)
}

func (a Attestation) payload() []byte {
	var w writer
	switch a.Kind {
	case AttPending:
		w.varbytes([]byte(a.URI))
	case AttBitcoin:
		w.varuint(a.Height)
	default:
		w.raw(a.Payload)
	}
	return w.b
}

// Edge is an operation and the node it leads to.
type Edge struct {
	Op   Op
	Next *Node
}

// Node is a message and what is known about it: attestations that commit to
// it and operations that derive other messages from it.
type Node struct {
	Msg          []byte
	Attestations []Attestation
	Edges        []Edge
}

// NewNode returns an empty node for msg.
func NewNode(msg []byte) *Node { return &Node{Msg: append([]byte(nil), msg...)} }

// AddAttestation adds a (deduplicated).
func (n *Node) AddAttestation(a Attestation) {
	for _, x := range n.Attestations {
		if compareAtt(x, a) == 0 {
			return
		}
	}
	n.Attestations = append(n.Attestations, a)
}

// Child returns the node reached by op, creating it (with the result of op)
// when the node has no such edge yet.
func (n *Node) Child(op Op) (*Node, error) {
	for _, e := range n.Edges {
		if compareOps(e.Op, op) == 0 {
			return e.Next, nil
		}
	}
	res, err := op.Apply(n.Msg)
	if err != nil {
		return nil, err
	}
	c := &Node{Msg: res}
	n.Edges = append(n.Edges, Edge{Op: Op{Tag: op.Tag, Arg: append([]byte(nil), op.Arg...)}, Next: c})
	return c, nil
}

// Count returns the number of nodes of the tree.
func (n *Node) Count() int {
	c := 1
	for _, e := range n.Edges {
		c += e.Next.Count()
	}
	return c
}

// Walk calls f for every node, parents first, in the canonical order.
func (n *Node) Walk(f func(*Node)) {
	f(n)
	n.sort()
	for _, e := range n.Edges {
		e.Next.Walk(f)
	}
}

func (n *Node) sort() {
	sort.SliceStable(n.Attestations, func(i, j int) bool { return compareAtt(n.Attestations[i], n.Attestations[j]) < 0 })
	sort.SliceStable(n.Edges, func(i, j int) bool { return compareOps(n.Edges[i].Op, n.Edges[j].Op) < 0 })
}

// Merge adds every operation and attestation of other to n. Both must be for
// the same message. The result is limited to MaxNodes nodes and MaxDepth levels.
func (n *Node) Merge(other *Node) error {
	if err := n.merge(other, 1); err != nil {
		return err
	}
	if n.Count() > MaxNodes {
		return ErrNodeBudget
	}
	return nil
}

func (n *Node) merge(other *Node, depth int) error {
	if depth > MaxDepth {
		return ErrDepth
	}
	if !bytes.Equal(n.Msg, other.Msg) {
		return fmt.Errorf("%w: cannot merge nodes for different messages", ErrMalformed)
	}
	for _, a := range other.Attestations {
		n.AddAttestation(a)
	}
	for _, e := range other.Edges {
		var mine *Node
		for _, m := range n.Edges {
			if compareOps(m.Op, e.Op) == 0 {
				mine = m.Next
				break
			}
		}
		if mine == nil {
			mine = &Node{Msg: append([]byte(nil), e.Next.Msg...)}
			n.Edges = append(n.Edges, Edge{Op: Op{Tag: e.Op.Tag, Arg: append([]byte(nil), e.Op.Arg...)}, Next: mine})
		}
		if err := mine.merge(e.Next, depth+1); err != nil {
			return err
		}
	}
	return nil
}

// Clone returns a deep copy.
func (n *Node) Clone() *Node {
	c := &Node{Msg: append([]byte(nil), n.Msg...)}
	c.Attestations = append(c.Attestations, n.Attestations...)
	for _, e := range n.Edges {
		c.Edges = append(c.Edges, Edge{Op: Op{Tag: e.Op.Tag, Arg: append([]byte(nil), e.Op.Arg...)}, Next: e.Next.Clone()})
	}
	return c
}

// Serialize encodes the tree in the canonical order of the reference library:
// attestations sorted, then operations sorted; every item but the last
// preceded by 0xff; an attestation introduced by 0x00. A node with neither
// is an error.
func (n *Node) Serialize() ([]byte, error) {
	var w writer
	if err := n.serialize(&w, 1); err != nil {
		return nil, err
	}
	return w.b, nil
}

func (n *Node) serialize(w *writer, depth int) error {
	if depth > MaxDepth {
		return ErrDepth
	}
	if len(n.Attestations) == 0 && len(n.Edges) == 0 {
		return ErrEmptyNode
	}
	n.sort()
	atts := n.Attestations
	if len(n.Edges) == 0 {
		for _, a := range atts[:len(atts)-1] {
			w.byte(0xff)
			w.attestation(a)
		}
		w.attestation(atts[len(atts)-1])
		return nil
	}
	for _, a := range atts {
		w.byte(0xff)
		w.attestation(a)
	}
	for i, e := range n.Edges {
		if i < len(n.Edges)-1 {
			w.byte(0xff)
		}
		w.op(e.Op)
		if err := e.Next.serialize(w, depth+1); err != nil {
			return err
		}
	}
	return nil
}

type writer struct{ b []byte }

func (w *writer) byte(c byte)  { w.b = append(w.b, c) }
func (w *writer) raw(p []byte) { w.b = append(w.b, p...) }
func (w *writer) varuint(v uint64) {
	for {
		c := byte(v & 0x7f)
		if v > 0x7f {
			c |= 0x80
		}
		w.b = append(w.b, c)
		if v <= 0x7f {
			return
		}
		v >>= 7
	}
}
func (w *writer) varbytes(p []byte) { w.varuint(uint64(len(p))); w.raw(p) }
func (w *writer) op(o Op) {
	w.byte(o.Tag)
	if o.binary() {
		w.varbytes(o.Arg)
	}
}
func (w *writer) attestation(a Attestation) {
	w.byte(0x00)
	w.raw(a.Tag[:])
	w.varbytes(a.payload())
}

type reader struct {
	b   []byte
	pos int
}

func (r *reader) byte() (byte, error) {
	if r.pos >= len(r.b) {
		return 0, ErrTruncated
	}
	c := r.b[r.pos]
	r.pos++
	return c, nil
}

func (r *reader) bytes(n int) ([]byte, error) {
	if n < 0 || n > len(r.b)-r.pos {
		return nil, ErrTruncated
	}
	p := r.b[r.pos : r.pos+n]
	r.pos += n
	return p, nil
}

func (r *reader) varuint() (uint64, error) {
	var v uint64
	for i := 0; i < MaxVarUintBytes; i++ {
		c, err := r.byte()
		if err != nil {
			return 0, err
		}
		v |= uint64(c&0x7f) << (7 * uint(i))
		if c&0x80 == 0 {
			return v, nil
		}
	}
	return 0, fmt.Errorf("%w: varuint longer than %d bytes", ErrMalformed, MaxVarUintBytes)
}

func (r *reader) varbytes(max, min int) ([]byte, error) {
	l, err := r.varuint()
	if err != nil {
		return nil, err
	}
	if l > uint64(max) {
		return nil, fmt.Errorf("%w: length %d exceeds the limit %d", ErrMalformed, l, max)
	}
	if int(l) < min {
		return nil, fmt.Errorf("%w: length %d below the minimum %d", ErrMalformed, l, min)
	}
	return r.bytes(int(l))
}

func (r *reader) attestation() (Attestation, error) {
	tagb, err := r.bytes(8)
	if err != nil {
		return Attestation{}, err
	}
	var tag [8]byte
	copy(tag[:], tagb)
	payload, err := r.varbytes(MaxPayloadLen, 0)
	if err != nil {
		return Attestation{}, err
	}
	pr := &reader{b: payload}
	switch tag {
	case TagPending:
		u, err := pr.varbytes(MaxURILen, 0)
		if err != nil {
			return Attestation{}, err
		}
		if err := checkURI(u); err != nil {
			return Attestation{}, err
		}
		if pr.pos != len(payload) {
			return Attestation{}, fmt.Errorf("%w: data after the pending attestation uri", ErrMalformed)
		}
		return Attestation{Kind: AttPending, URI: string(u), Tag: tag}, nil
	case TagBitcoin:
		h, err := pr.varuint()
		if err != nil {
			return Attestation{}, err
		}
		if h > MaxHeight {
			return Attestation{}, fmt.Errorf("%w: block height %d too large", ErrMalformed, h)
		}
		if pr.pos != len(payload) {
			return Attestation{}, fmt.Errorf("%w: data after the bitcoin attestation height", ErrMalformed)
		}
		return Attestation{Kind: AttBitcoin, Height: h, Tag: tag}, nil
	}
	return Attestation{Kind: AttUnknown, Tag: tag, Payload: append([]byte(nil), payload...)}, nil
}

type parser struct {
	r     reader
	nodes int
}

func (p *parser) op(tag byte) (Op, error) {
	switch tag {
	case TagSHA1, TagRIPEMD160, TagSHA256:
		return Op{Tag: tag}, nil
	case TagAppend, TagPrepend:
		arg, err := p.r.varbytes(MaxMessageLen, 1)
		if err != nil {
			return Op{}, err
		}
		return Op{Tag: tag, Arg: append([]byte(nil), arg...)}, nil
	}
	return Op{}, opError(tag)
}

func (p *parser) node(msg []byte, depth int) (*Node, error) {
	if depth > MaxDepth {
		return nil, ErrDepth
	}
	p.nodes++
	if p.nodes > MaxNodes {
		return nil, ErrNodeBudget
	}
	n := &Node{Msg: msg}
	item := func(tag byte) error {
		if tag == 0x00 {
			a, err := p.r.attestation()
			if err != nil {
				return err
			}
			n.AddAttestation(a)
			return nil
		}
		op, err := p.op(tag)
		if err != nil {
			return err
		}
		for _, e := range n.Edges {
			if compareOps(e.Op, op) == 0 {
				return fmt.Errorf("%w: operation %s repeated", ErrMalformed, op.Name())
			}
		}
		res, err := op.Apply(msg)
		if err != nil {
			return fmt.Errorf("%w: %v", ErrMalformed, err)
		}
		child, err := p.node(res, depth+1)
		if err != nil {
			return err
		}
		n.Edges = append(n.Edges, Edge{Op: op, Next: child})
		return nil
	}
	tag, err := p.r.byte()
	if err != nil {
		return nil, err
	}
	for tag == 0xff {
		t, err := p.r.byte()
		if err != nil {
			return nil, err
		}
		if err := item(t); err != nil {
			return nil, err
		}
		if tag, err = p.r.byte(); err != nil {
			return nil, err
		}
	}
	if err := item(tag); err != nil {
		return nil, err
	}
	return n, nil
}

// ParseFragment parses a serialized timestamp (the answer of a calendar, or a
// stored fragment) whose root message is msg. The whole input must be used.
func ParseFragment(b, msg []byte) (*Node, error) {
	if len(b) > MaxProofSize {
		return nil, ErrTooLarge
	}
	if len(msg) == 0 || len(msg) > MaxMessageLen {
		return nil, fmt.Errorf("%w: root message must have 1 to %d bytes", ErrMalformed, MaxMessageLen)
	}
	p := &parser{r: reader{b: b}}
	n, err := p.node(append([]byte(nil), msg...), 1)
	if err != nil {
		return nil, err
	}
	if p.r.pos != len(b) {
		return nil, ErrTrailing
	}
	return n, nil
}

// magic is the header of a detached timestamp file (31 bytes).
const magic = "\x00OpenTimestamps\x00\x00Proof\x00\xbf\x89\xe2\xe8\x84\xe8\x92\x94"

// Magic returns a copy of the 31 bytes that start a detached timestamp file.
func Magic() []byte { return []byte(magic) }

// MajorVersion is the only supported major version of the file format.
const MajorVersion = 1

// File is a detached timestamp file.
type File struct {
	HashOp byte  // TagSHA1, TagRIPEMD160 or TagSHA256
	Root   *Node // Root.Msg is the file digest
}

// ParseFile parses a detached timestamp file (.ots). The input is limited to
// MaxProofSize bytes and must be used entirely.
func ParseFile(b []byte) (*File, error) {
	if len(b) > MaxProofSize {
		return nil, ErrTooLarge
	}
	r := reader{b: b}
	m, err := r.bytes(len(magic))
	if err != nil || string(m) != magic {
		return nil, ErrBadMagic
	}
	v, err := r.byte()
	if err != nil {
		return nil, err
	}
	if v != MajorVersion {
		return nil, fmt.Errorf("%w: %d", ErrVersion, v)
	}
	tag, err := r.byte()
	if err != nil {
		return nil, err
	}
	dl := DigestLen(tag)
	if dl == 0 {
		if _, known := refusedOps[tag]; known || tag == TagAppend || tag == TagPrepend {
			return nil, fmt.Errorf("%w: %s", ErrBadHashOp, Op{Tag: tag}.Name())
		}
		return nil, fmt.Errorf("%w: tag 0x%02x", ErrBadHashOp, tag)
	}
	digest, err := r.bytes(dl)
	if err != nil {
		return nil, err
	}
	p := &parser{r: r}
	root, err := p.node(append([]byte(nil), digest...), 1)
	if err != nil {
		return nil, err
	}
	if p.r.pos != len(b) {
		return nil, ErrTrailing
	}
	return &File{HashOp: tag, Root: root}, nil
}

// Serialize encodes the file.
func (f *File) Serialize() ([]byte, error) {
	dl := DigestLen(f.HashOp)
	if dl == 0 || len(f.Root.Msg) != dl {
		return nil, ErrBadHashOp
	}
	body, err := f.Root.Serialize()
	if err != nil {
		return nil, err
	}
	out := []byte(magic)
	out = append(out, MajorVersion, f.HashOp)
	out = append(out, f.Root.Msg...)
	out = append(out, body...)
	if len(out) > MaxProofSize {
		return nil, ErrTooLarge
	}
	return out, nil
}

// Leaf is an attestation with the message it commits to.
type Leaf struct {
	Msg []byte
	Att Attestation
}

// Leaves returns every attestation of the tree with its message, in canonical order.
func (n *Node) Leaves() []Leaf {
	var out []Leaf
	n.Walk(func(x *Node) {
		for _, a := range x.Attestations {
			out = append(out, Leaf{Msg: x.Msg, Att: a})
		}
	})
	return out
}
