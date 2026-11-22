// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package staging

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

const (
	keyMagic    = "KDXMK001"
	keyIDBytes  = 8
	keyBytes    = 32
	keyFileSize = len(keyMagic) + keyIDBytes + keyBytes + sha256.Size
)

// Keys is the master key. It wraps and unwraps the data keys of the files; it never touches a file's content.
type Keys struct {
	id   string
	aead cipher.AEAD
}

// ID is the id of the master key (16 hex characters), kept in the row of every file it wrapped.
func (k *Keys) ID() string { return k.id }

func newKeys(id [keyIDBytes]byte, key []byte) (*Keys, error) {
	b, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	g, err := cipher.NewGCM(b)
	if err != nil {
		return nil, err
	}
	return &Keys{id: hex.EncodeToString(id[:]), aead: g}, nil
}

// NewKeysForTest makes a master key from bytes, for tests of callers that need no file.
func NewKeysForTest(seed byte) *Keys {
	var id [keyIDBytes]byte
	for i := range id {
		id[i] = seed
	}
	key := bytes.Repeat([]byte{seed}, keyBytes)
	k, err := newKeys(id, key)
	if err != nil {
		panic(err)
	}
	return k
}

func wrapAAD(transferID int64, keyID string) []byte {
	b := make([]byte, 8, 8+len(keyID))
	binary.BigEndian.PutUint64(b, uint64(transferID)) //nolint:gosec // an id is positive
	return append(b, keyID...)
}

// NewFileKey makes a random data key for a file and its wrapped form for the row. The wrapping is bound to the transfer id and the key id.
func (k *Keys) NewFileKey(transferID int64) (dataKey, wrapped []byte, err error) {
	dataKey = make([]byte, keyBytes)
	if _, err = io.ReadFull(rand.Reader, dataKey); err != nil {
		return nil, nil, err
	}
	nonce := make([]byte, k.aead.NonceSize())
	if _, err = io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, nil, err
	}
	wrapped = k.aead.Seal(nonce, nonce, dataKey, wrapAAD(transferID, k.id))
	return dataKey, wrapped, nil
}

// Unwrap opens the wrapped data key of a row. A key id that is not this master key's, and a wrapped key that does not open, give
// ErrKeyLost: the file cannot be read and the row is to be marked lost with the code transfer_key_lost.
func (k *Keys) Unwrap(transferID int64, keyID string, wrapped []byte) ([]byte, error) {
	if keyID != k.id {
		return nil, ErrKeyLost
	}
	ns := k.aead.NonceSize()
	if len(wrapped) != ns+keyBytes+k.aead.Overhead() {
		return nil, ErrKeyLost
	}
	dk, err := k.aead.Open(nil, wrapped[:ns], wrapped[ns:], wrapAAD(transferID, keyID))
	if err != nil {
		return nil, ErrKeyLost
	}
	return dk, nil
}

func encodeKeyFile(id [keyIDBytes]byte, key []byte) []byte {
	b := make([]byte, 0, keyFileSize)
	b = append(b, keyMagic...)
	b = append(b, id[:]...)
	b = append(b, key...)
	sum := sha256.Sum256(b)
	return append(b, sum[:]...)
}

func parseKeyFile(b []byte) (*Keys, error) {
	if len(b) != keyFileSize || string(b[:len(keyMagic)]) != keyMagic {
		return nil, fmt.Errorf("%w: format", ErrKeyFile)
	}
	body := b[:keyFileSize-sha256.Size]
	sum := sha256.Sum256(body)
	if !bytes.Equal(sum[:], b[keyFileSize-sha256.Size:]) {
		return nil, fmt.Errorf("%w: checksum", ErrKeyFile)
	}
	var id [keyIDBytes]byte
	copy(id[:], body[len(keyMagic):])
	return newKeys(id, body[len(keyMagic)+keyIDBytes:])
}

// readKeyFile reads the key file with every check: a regular file, no link, mode 0600 or tighter and owned by this user (where the system
// has them), and the exact size and checksum.
func readKeyFile(fsys FS, path string) (*Keys, error) {
	info, err := fsys.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%w: not a regular file", ErrKeyFile)
	}
	if modesEnforced && (info.Mode().Perm()&0o077 != 0 || !ownedByUs(info)) {
		return nil, fmt.Errorf("%w: key file", ErrDirMode)
	}
	if info.Size() != int64(keyFileSize) {
		return nil, fmt.Errorf("%w: size", ErrKeyFile)
	}
	f, err := fsys.OpenFile(path, os.O_RDONLY, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, int64(keyFileSize)+1))
	if err != nil {
		return nil, err
	}
	return parseKeyFile(b)
}

// LoadOrCreateKeys reads the master key at path, or makes it at first use.
//
//   - The file must be outside stagingDir (ErrKeyFile).
//   - An existing file is checked for kind, mode, owner, size and checksum at every call.
//   - A missing file is created only if filesExist reports false. When files under an old key exist it returns ErrKeyLost and creates
//     nothing, so a deleted key can never be replaced silently (DXR-24). filesExist is asked after the file was found missing.
//   - Creation writes a temporary file with O_EXCL, syncs it, and hard-links it to path; a link cannot replace a file, so of two first uses
//     at once exactly one key exists and the other use reads it. The result is read back before it is returned.
func LoadOrCreateKeys(fsys FS, path, stagingDir string, filesExist func() (bool, error)) (*Keys, error) {
	if fsys == nil {
		fsys = OSFS{}
	}
	if !filepath.IsAbs(path) {
		return nil, fmt.Errorf("%w: the path must be absolute", ErrKeyFile)
	}
	path = filepath.Clean(path)
	if stagingDir != "" {
		sd := filepath.Clean(stagingDir)
		if inside(path, sd) || path == sd {
			return nil, fmt.Errorf("%w: it is inside the staging directory", ErrKeyFile)
		}
	}
	k, err := readKeyFile(fsys, path)
	if err == nil {
		return k, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	if filesExist != nil {
		exist, ferr := filesExist()
		if ferr != nil {
			return nil, ferr
		}
		if exist {
			return nil, ErrKeyLost
		}
	}
	var id [keyIDBytes]byte
	key := make([]byte, keyBytes)
	if _, err := io.ReadFull(rand.Reader, id[:]); err != nil {
		return nil, err
	}
	if _, err := io.ReadFull(rand.Reader, key); err != nil {
		return nil, err
	}
	var rnd [8]byte
	if _, err := io.ReadFull(rand.Reader, rnd[:]); err != nil {
		return nil, err
	}
	tmp := path + ".tmp-" + hex.EncodeToString(rnd[:])
	f, err := fsys.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, fmt.Errorf("staging: %w", err)
	}
	_, werr := f.Write(encodeKeyFile(id, key))
	if werr == nil {
		werr = f.Sync()
	}
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		_ = fsys.Remove(tmp)
		return nil, fmt.Errorf("staging: %w", werr)
	}
	lerr := fsys.Link(tmp, path)
	_ = fsys.Remove(tmp)
	if lerr != nil && !errors.Is(lerr, fs.ErrExist) && !strings.Contains(strings.ToLower(lerr.Error()), "exist") {
		return nil, fmt.Errorf("staging: %w", lerr)
	}
	// Whoever won, the file at path is the key; read it back with every check.
	k, err = readKeyFile(fsys, path)
	if err != nil {
		return nil, err
	}
	if lerr == nil && k.id != hex.EncodeToString(id[:]) {
		return nil, fmt.Errorf("%w: the file was replaced while it was created", ErrKeyFile)
	}
	return k, nil
}
