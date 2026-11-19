// SPDX-License-Identifier: AGPL-3.0-only

package ledgersign

import (
	"crypto/ed25519"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/ledgerkeys"
)

var fingerprintRE = regexp.MustCompile(`^[0-9a-f]{64}$`)

// ErrNoKeyFile means the server has no private key file for a fingerprint: it is not a holder, or the
// directory was restored without the keys.
var ErrNoKeyFile = errors.New("ledgersign: this server has no private key file for the key")

// ErrPassphrase means a key file cannot be opened with the passphrase this server has. The text
// never holds the passphrase.
var ErrPassphrase = errors.New("ledgersign: the key file cannot be opened with this server's passphrase file")

// Keyring is the private key files of this server: <dir>/signing/<fingerprint>.key, mode 0600, written
// with the atomic procedure of internal/ledgerkeys, plain or sealed with the passphrase file (design
// 3.2.2). It keeps the opened signers in memory (the one object that holds a seed, which exposes Sign
// only); the passphrase is read from its file whenever a file is read or written and destroyed at once,
// so it is not kept in memory longer than needed.
type Keyring struct {
	dir      string
	passFile string

	mu    sync.Mutex
	cache map[string]*ledgerkeys.FileSigner
}

// NewKeyring prepares the keyring below dir (the --ledger-keys-dir); passFile is the passphrase file
// ("" for key files in clear text). It creates <dir>/signing with mode 0700.
func NewKeyring(dir, passFile string) (*Keyring, error) {
	if dir == "" {
		return nil, errors.New("ledgersign: the key directory is empty")
	}
	k := &Keyring{dir: dir, passFile: passFile, cache: map[string]*ledgerkeys.FileSigner{}}
	if err := os.MkdirAll(k.signingDir(), 0o700); err != nil {
		return nil, fmt.Errorf("ledgersign: creating the key directory: %w", err)
	}
	k.CleanTemp(time.Minute)
	return k, nil
}

func (k *Keyring) signingDir() string { return filepath.Join(k.dir, "signing") }

func (k *Keyring) path(fp string) (string, error) {
	if !fingerprintRE.MatchString(fp) {
		return "", fmt.Errorf("ledgersign: %q is not a key fingerprint", clipText(fp))
	}
	return filepath.Join(k.signingDir(), fp+".key"), nil
}

func clipText(s string) string {
	if len(s) > 40 {
		return s[:40] + "..."
	}
	return s
}

// passphrase reads the passphrase file; the caller destroys the result. The zero Passphrase (no file
// configured) makes plain files.
func (k *Keyring) passphrase() (ledgerkeys.Passphrase, error) {
	if k.passFile == "" {
		return ledgerkeys.Passphrase{}, nil
	}
	p, err := ledgerkeys.ReadPassphraseFile(k.passFile)
	if err != nil {
		return ledgerkeys.Passphrase{}, fmt.Errorf("ledgersign: reading the passphrase file: %w", err)
	}
	return p, nil
}

// Sealed reports whether this server has a passphrase file configured.
func (k *Keyring) Sealed() bool { return k.passFile != "" }

// Generate makes a new key and returns its public key and a function that writes its file (the caller
// decides whether the key enters the set first, so a key that collides leaves no file behind). The
// signer is closed by the function (and by a call of the returned close function when the key is dropped).
func (k *Keyring) Generate() (pub ed25519.PublicKey, write func() error, drop func(), err error) {
	s, err := ledgerkeys.GenerateKey(nil)
	if err != nil {
		return nil, nil, nil, err
	}
	pub = s.Public()
	fp := ledgerkeys.Fingerprint(pub)
	write = func() error {
		defer func() { _ = s.Close() }()
		p, err := k.passphrase()
		if err != nil {
			return err
		}
		defer p.Destroy()
		path, err := k.path(fp)
		if err != nil {
			return err
		}
		if err := ledgerkeys.WriteFile(path, *s, p, false); err != nil {
			return err
		}
		return nil
	}
	return pub, write, func() { _ = s.Close() }, nil
}

// Has reports whether the file of a key exists on this server.
func (k *Keyring) Has(fp string) bool {
	path, err := k.path(fp)
	if err != nil {
		return false
	}
	st, err := os.Lstat(path)
	return err == nil && st.Mode().IsRegular()
}

// Signer opens the key (once; the signer stays in memory until Forget or Close).
func (k *Keyring) Signer(fp string) (ledgerkeys.Signer, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if s, ok := k.cache[fp]; ok {
		return s, nil
	}
	path, err := k.path(fp)
	if err != nil {
		return nil, err
	}
	if !k.Has(fp) {
		return nil, ErrNoKeyFile
	}
	p, err := k.passphrase()
	if err != nil {
		return nil, err
	}
	defer p.Destroy()
	s, err := ledgerkeys.ReadNamed(k.signingDir(), fp, p)
	if err != nil {
		if errors.Is(err, ledgerkeys.ErrWrongPassphrase) || errors.Is(err, ledgerkeys.ErrPassphraseRequired) || errors.Is(err, ledgerkeys.ErrPassphraseNotNeeded) {
			return nil, ErrPassphrase
		}
		_ = path
		return nil, fmt.Errorf("ledgersign: the key file of %s cannot be opened: %s", fp[:16], k.noPath(err))
	}
	k.cache[fp] = s
	return s, nil
}

// Forget closes and drops the opened signer of a key.
func (k *Keyring) Forget(fp string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if s, ok := k.cache[fp]; ok {
		_ = s.Close()
		delete(k.cache, fp)
	}
}

// Close zeroes every opened signer.
func (k *Keyring) Close() {
	k.mu.Lock()
	defer k.mu.Unlock()
	for fp, s := range k.cache {
		_ = s.Close()
		delete(k.cache, fp)
	}
}

// File returns the key file as it is stored (plain or sealed), for the install call to a holder. The
// bytes hold the seed in clear when the file is plain: the caller sends them only over the mutually
// authenticated internal API and zeroes them.
func (k *Keyring) File(fp string) ([]byte, error) {
	path, err := k.path(fp)
	if err != nil {
		return nil, err
	}
	if !k.Has(fp) {
		return nil, ErrNoKeyFile
	}
	data, err := os.ReadFile(path) //nolint:gosec // the path is the fingerprint below the key directory
	if err != nil {
		return nil, fmt.Errorf("ledgersign: reading the key file: %w", pathError(err))
	}
	return data, nil
}

// pathError keeps the file name out of nothing: a path is not a secret, but the text of an error from
// the file system is reduced to its cause.
func pathError(err error) error {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return pe.Err
	}
	return err
}

// Install writes a key file received from the leader. The file is opened with this server's passphrase
// (a sealed file made with another passphrase is refused: ErrPassphrase) and written again under this
// server's own sealing, so a holder never keeps a file it could not open. Installing a key that is
// there is a no-op when the key is the same one.
func (k *Keyring) Install(fp string, data []byte) error {
	path, err := k.path(fp)
	if err != nil {
		return err
	}
	p, err := k.passphrase()
	if err != nil {
		return err
	}
	defer p.Destroy()
	f, err := ledgerkeys.Parse(data)
	if err != nil {
		return fmt.Errorf("ledgersign: the key file is not valid: %w", err)
	}
	defer f.Destroy()
	// A plain file needs no passphrase, whatever this server uses for its own files; a sealed one needs
	// this server's passphrase.
	open := p
	if !f.Encrypted() {
		open = ledgerkeys.Passphrase{}
	}
	s, err := f.Open(open)
	if err != nil {
		if errors.Is(err, ledgerkeys.ErrWrongPassphrase) || errors.Is(err, ledgerkeys.ErrPassphraseRequired) || errors.Is(err, ledgerkeys.ErrPassphraseNotNeeded) {
			return ErrPassphrase
		}
		return fmt.Errorf("ledgersign: the key file is not valid: %w", err)
	}
	defer func() { _ = s.Close() }()
	if s.Fingerprint() != fp {
		return errors.New("ledgersign: the key in the file has another fingerprint than the call says")
	}
	if k.Has(fp) {
		have, err := ledgerkeys.ReadNamed(k.signingDir(), fp, p)
		if err != nil {
			// A file that exists and does not open is no copy of the key: it is replaced by this one (P7-04).
			if rerr := k.Remove(fp); rerr != nil {
				return rerr
			}
			return ledgerkeys.WriteFile(path, *s, p, false)
		}
		same := have.SameKey(*s)
		_ = have.Close()
		if !same {
			return errors.New("ledgersign: another key file exists under this fingerprint")
		}
		return nil
	}
	return ledgerkeys.WriteFile(path, *s, p, false)
}

// Remove deletes the key file of a key and forgets its signer. The content is overwritten with zeros
// before the file is removed (best effort: a journaling file system or a flash disk may keep copies).
// A key that is not there is not an error.
func (k *Keyring) Remove(fp string) error {
	path, err := k.path(fp)
	if err != nil {
		return err
	}
	k.Forget(fp)
	_ = os.Remove(filepath.Join(k.signingDir(), fp+".task"))
	st, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return pathError(err)
	}
	if st.Mode().IsRegular() {
		if f, err := os.OpenFile(path, os.O_WRONLY, 0); err == nil { //nolint:gosec // the key file
			_, _ = f.Write(make([]byte, st.Size()))
			_ = f.Sync()
			_ = f.Close()
		}
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("ledgersign: removing the key file: %w", pathError(err))
	}
	return nil
}

// List returns the fingerprints of the key files on this server, sorted.
func (k *Keyring) List() ([]string, error) {
	ents, err := os.ReadDir(k.signingDir())
	if err != nil {
		return nil, fmt.Errorf("ledgersign: reading the key directory: %w", pathError(err))
	}
	var out []string
	for _, e := range ents {
		name := e.Name()
		if e.Type().IsRegular() && strings.HasSuffix(name, ".key") && fingerprintRE.MatchString(strings.TrimSuffix(name, ".key")) {
			out = append(out, strings.TrimSuffix(name, ".key"))
		}
	}
	sort.Strings(out)
	return out, nil
}

// Protected reports whether every key file of this server is sealed (true with no file at all when a
// passphrase file is configured: the next one will be).
func (k *Keyring) Protected() (bool, error) {
	if k.passFile == "" {
		return false, nil
	}
	fps, err := k.List()
	if err != nil {
		return false, err
	}
	for _, fp := range fps {
		data, err := k.File(fp)
		if err != nil {
			return false, err
		}
		f, err := ledgerkeys.Parse(data)
		clear(data)
		if err != nil {
			return false, nil
		}
		enc := f.Encrypted()
		f.Destroy()
		if !enc {
			return false, nil
		}
	}
	return true, nil
}

// Orphan finds a key file that has no row, made after since: the file a task wrote before it was killed
// and could record it. known are the fingerprints of the rows. It returns "" when there is none. A file
// older than since (less slack, the difference the clocks of the server and the database may have) is never
// taken: it may be the only copy of a key of a restored system database.
func (k *Keyring) Orphan(known map[string]bool, since time.Time, slack time.Duration) (string, error) {
	fps, err := k.List()
	if err != nil {
		return "", err
	}
	for _, fp := range fps {
		if known[fp] || !k.marked(fp) {
			continue
		}
		st, err := os.Stat(filepath.Join(k.signingDir(), fp+".key"))
		if err != nil {
			continue
		}
		if !st.ModTime().Before(since.Add(-slack)) {
			return fp, nil
		}
	}
	return "", nil
}

// PublicOf opens a key file and returns its public key (the file may be sealed).
func (k *Keyring) PublicOf(fp string) (ed25519.PublicKey, error) {
	s, err := k.Signer(fp)
	if err != nil {
		return nil, err
	}
	return s.Public(), nil
}

// Verify opens the key file of a key (from the disk, not from the cache) and checks that it holds the public key that the row of the key has.
// An error names the fingerprint, never a path (P7-04, P7-08).
func (k *Keyring) Verify(fp string, pub ed25519.PublicKey) error {
	if !k.Has(fp) {
		return ErrNoKeyFile
	}
	p, err := k.passphrase()
	if err != nil {
		return err
	}
	defer p.Destroy()
	s, err := ledgerkeys.ReadNamed(k.signingDir(), fp, p)
	if err != nil {
		if errors.Is(err, ledgerkeys.ErrWrongPassphrase) || errors.Is(err, ledgerkeys.ErrPassphraseRequired) || errors.Is(err, ledgerkeys.ErrPassphraseNotNeeded) {
			return ErrPassphrase
		}
		return fmt.Errorf("ledgersign: the key file of %s cannot be opened: %s", fp[:16], k.noPath(err))
	}
	defer func() { _ = s.Close() }()
	if !s.Public().Equal(pub) {
		return fmt.Errorf("ledgersign: the key file of %s holds another key", fp[:16])
	}
	return nil
}

// noPath is the text of an error without the key directory.
func (k *Keyring) noPath(err error) string {
	return strings.ReplaceAll(strings.ReplaceAll(err.Error(), k.signingDir(), "<key directory>"), k.dir, "<key directory>")
}

// ListChecked lists the key files that open (with this server's passphrase) and the ones that exist and do not.
func (k *Keyring) ListChecked() (ok, bad []string, err error) {
	all, err := k.List()
	if err != nil {
		return nil, nil, err
	}
	p, perr := k.passphrase()
	for _, fp := range all {
		if perr != nil {
			bad = append(bad, fp)
			continue
		}
		s, rerr := ledgerkeys.ReadNamed(k.signingDir(), fp, p)
		if rerr != nil {
			bad = append(bad, fp)
			continue
		}
		_ = s.Close()
		ok = append(ok, fp)
	}
	if perr == nil {
		p.Destroy()
	}
	return ok, bad, nil
}

// CleanTemp removes the temporary files a crash left in the key directory (a key file is written under a temporary name and linked into place): the
// ones older than maxAge. A temporary file can hold a whole key file (P7-09).
func (k *Keyring) CleanTemp(maxAge time.Duration) {
	ents, err := os.ReadDir(k.signingDir())
	if err != nil {
		return
	}
	for _, e := range ents {
		if !strings.HasPrefix(e.Name(), ".key-") || !e.Type().IsRegular() {
			continue
		}
		if info, err := e.Info(); err == nil && time.Since(info.ModTime()) >= maxAge {
			_ = os.Remove(filepath.Join(k.signingDir(), e.Name()))
		}
	}
}

// Mark writes the marker that says which task made a key file: a file with no row is adopted only when it carries the marker of the task that asks
// (Orphan). The marker is written by the daemon after the key file; a file that somebody placed has none.
func (k *Keyring) Mark(fp string, taskID int64) error {
	if _, err := k.path(fp); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(k.signingDir(), fp+".task"), []byte(fmt.Sprint(taskID)+"\n"), 0o600)
}

func (k *Keyring) marked(fp string) bool {
	b, err := os.ReadFile(filepath.Join(k.signingDir(), fp+".task"))
	if err != nil {
		return false
	}
	_, err = strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	return err == nil
}
