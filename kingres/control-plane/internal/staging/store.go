// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package staging

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Defaults of the limits (design, "Where files live").
const (
	GiB = int64(1) << 30
	// DefaultCheckEvery is how many bytes a writer seals between two looks at the free space of the file system.
	DefaultCheckEvery = int64(64) << 20
	// DefaultMinAge is the age under which the janitor deletes nothing.
	DefaultMinAge = time.Hour
)

// DefaultQuota is the smaller of 100 GiB and a quarter of the file system.
func DefaultQuota(total uint64) int64 {
	q := int64(total / 4) //nolint:gosec // a file system is smaller than 2^63 bytes
	if q > 100*GiB || q < 0 {
		q = 100 * GiB
	}
	return q
}

// DefaultReserve is the larger of 5 GiB and a tenth of the file system.
func DefaultReserve(total uint64) uint64 {
	r := total / 10
	if r < uint64(5*GiB) {
		r = uint64(5 * GiB)
	}
	return r
}

// Config is what Open needs.
type Config struct {
	// FS defaults to OSFS.
	FS FS
	// Dir is the staging directory, an absolute path. It is created (mode 0700, parent must exist) when it does not exist.
	Dir string
	// Forbidden are directories that the staging directory may not be or contain: the data directory of kingresd, the data
	// directories of PostgreSQL. The root is always forbidden.
	Forbidden []string
	// Quota is the largest sum of the reservations. Zero means DefaultQuota of the file system.
	Quota int64
	// Reserve is the free space that must remain on the file system. Nil means DefaultReserve.
	Reserve func(total uint64) uint64
	// MaxFileBytes is the largest plaintext of one file. Zero means no limit here.
	MaxFileBytes int64
	// MinAge is the age under which the janitor deletes nothing. Zero means DefaultMinAge.
	MinAge time.Duration
	// CheckEvery is the number of bytes between two looks at the free space. Zero means DefaultCheckEvery.
	CheckEvery int64
	// Now defaults to time.Now.
	Now func() time.Time
}

// Store is an open staging directory.
type Store struct {
	cfg  Config
	fs   FS
	dir  string
	mu   sync.Mutex
	res  map[int64]*Reservation
	open map[int64]bool
}

// Open checks the directory and returns the store. It is the one place that decides whether a directory may be staging space.
func Open(cfg Config) (*Store, error) {
	if cfg.FS == nil {
		cfg.FS = OSFS{}
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.MinAge <= 0 {
		cfg.MinAge = DefaultMinAge
	}
	if cfg.CheckEvery <= 0 {
		cfg.CheckEvery = DefaultCheckEvery
	}
	if cfg.Reserve == nil {
		cfg.Reserve = DefaultReserve
	}
	if !filepath.IsAbs(cfg.Dir) {
		return nil, fmt.Errorf("staging: the directory must be an absolute path")
	}
	dir := filepath.Clean(cfg.Dir)
	if filepath.Dir(dir) == dir {
		return nil, fmt.Errorf("%w: the root", ErrDirForbidden)
	}
	s := &Store{cfg: cfg, fs: cfg.FS, dir: dir, res: map[int64]*Reservation{}, open: map[int64]bool{}}

	info, err := s.fs.Lstat(dir)
	exists := err == nil
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("staging: %w", err)
	}
	if exists {
		if info.Mode()&fs.ModeSymlink != 0 {
			return nil, fmt.Errorf("%w: it is a symbolic link", ErrDirForbidden)
		}
		if !info.IsDir() {
			return nil, fmt.Errorf("staging: %q is not a directory", dir)
		}
	}
	if err := s.checkForbidden(dir, exists); err != nil {
		return nil, err
	}
	if !exists {
		if err := s.fs.Mkdir(dir, 0o700); err != nil {
			return nil, fmt.Errorf("staging: %w", err)
		}
		info, err = s.fs.Lstat(dir)
		if err != nil {
			return nil, fmt.Errorf("staging: %w", err)
		}
	}
	if modesEnforced && (info.Mode().Perm()&0o077 != 0 || !ownedByUs(info)) {
		return nil, fmt.Errorf("%w: directory", ErrDirMode)
	}
	if err := s.claim(); err != nil {
		return nil, err
	}
	return s, nil
}

// Dir is the directory.
func (s *Store) Dir() string { return s.dir }

func (s *Store) path(name string) string { return filepath.Join(s.dir, name) }

// checkForbidden refuses the root (done by the caller), a directory that is or contains a forbidden one, and a PostgreSQL data directory.
func (s *Store) checkForbidden(dir string, exists bool) error {
	forms := []string{dir}
	if exists {
		if real, err := s.fs.EvalSymlinks(dir); err == nil && real != dir {
			forms = append(forms, filepath.Clean(real))
		}
	}
	for _, f := range s.cfg.Forbidden {
		if f == "" {
			continue
		}
		fc := []string{filepath.Clean(f)}
		if real, err := s.fs.EvalSymlinks(f); err == nil {
			fc = append(fc, filepath.Clean(real))
		}
		for _, d := range forms {
			for _, x := range fc {
				if d == x || inside(x, d) {
					return fmt.Errorf("%w: it is or contains %q", ErrDirForbidden, f)
				}
			}
		}
	}
	if exists {
		for _, d := range forms {
			if _, err := s.fs.Lstat(filepath.Join(d, "PG_VERSION")); err == nil {
				return fmt.Errorf("%w: it is a PostgreSQL data directory", ErrDirForbidden)
			}
		}
	}
	return nil
}

// inside tells whether inner is strictly inside outer, by path only.
func inside(inner, outer string) bool {
	rel, err := filepath.Rel(outer, inner)
	if err != nil || rel == "." {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// readMarker returns nil when the marker is the one this program writes, fs.ErrNotExist when there is none, ErrBadMarker otherwise.
func (s *Store) readMarker() error {
	p := s.path(MarkerName)
	info, err := s.fs.Lstat(p)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() != int64(len(markerText)) {
		return ErrBadMarker
	}
	f, err := s.fs.OpenFile(p, os.O_RDONLY, 0)
	if err != nil {
		return ErrBadMarker
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, int64(len(markerText))+1))
	if err != nil || !bytes.Equal(b, []byte(markerText)) {
		return ErrBadMarker
	}
	return nil
}

// claim accepts a directory that carries the marker or is empty (and then writes the marker), and refuses every other.
func (s *Store) claim() error {
	err := s.readMarker()
	if err == nil {
		return nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	entries, rerr := s.fs.ReadDir(s.dir)
	if rerr != nil {
		return fmt.Errorf("staging: %w", rerr)
	}
	if len(entries) != 0 {
		return ErrDirNotMarked
	}
	f, werr := s.fs.OpenFile(s.path(MarkerName), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if werr != nil {
		if errors.Is(werr, fs.ErrExist) { // another start wrote it a moment ago
			return s.readMarker()
		}
		return fmt.Errorf("staging: %w", werr)
	}
	_, werr = f.Write([]byte(markerText))
	if werr == nil {
		werr = f.Sync()
	}
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		_ = s.fs.Remove(s.path(MarkerName))
		return fmt.Errorf("staging: %w", werr)
	}
	return nil
}

// FileState is what is on the disk for a transfer id.
type FileState int

// The states of a file.
const (
	StateNone  FileState = iota // no file
	StatePart                   // transfer-<id>.part
	StateFinal                  // transfer-<id>.kdx
)

// State says which file exists for the id and its size on the disk. A crash between the rename and the update of the row shows as
// StateFinal for a row that still says building.
func (s *Store) State(id int64) (FileState, int64, error) {
	if info, err := s.fs.Lstat(s.path(FinalName(id))); err == nil {
		if !info.Mode().IsRegular() {
			return StateNone, 0, ErrNotRegular
		}
		return StateFinal, info.Size(), nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return StateNone, 0, err
	}
	if info, err := s.fs.Lstat(s.path(PartName(id))); err == nil {
		if !info.Mode().IsRegular() {
			return StateNone, 0, ErrNotRegular
		}
		return StatePart, info.Size(), nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return StateNone, 0, err
	}
	return StateNone, 0, nil
}

// PartOffset is the plaintext offset of an upload that is being written, read from the file: the whole segments in it. A
// partial segment left by a crash does not count. It is never taken from a row (DXR-24).
func (s *Store) PartOffset(id int64) (int64, error) {
	st, size, err := s.State(id)
	if err != nil {
		return 0, err
	}
	if st != StatePart {
		return 0, ErrNotFound
	}
	if size < HeaderSize {
		return 0, ErrCorrupt
	}
	return ((size - HeaderSize) / SegmentDisk) * SegmentSize, nil
}

// Remove deletes the files of one transfer by the names derived from its id. It is what a failed export and a deleted export use; it
// is not the janitor and has no age rule. A file that is not regular is not removed and gives ErrNotRegular.
func (s *Store) Remove(id int64) error {
	var first error
	for _, name := range []string{PartName(id), FinalName(id)} {
		p := s.path(name)
		info, err := s.fs.Lstat(p)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			first = firstErr(first, err)
			continue
		}
		if !info.Mode().IsRegular() {
			first = firstErr(first, ErrNotRegular)
			continue
		}
		if err := s.fs.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
			first = firstErr(first, err)
		}
	}
	return first
}

func firstErr(a, b error) error {
	if a != nil {
		return a
	}
	return b
}

// Holds lists the reservations by id, for the report of a refusal and for tests.
func (s *Store) Holds() []Hold {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Hold, 0, len(s.res))
	for _, r := range s.res {
		out = append(out, Hold{ID: r.id, Bytes: r.reserved})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Reservation is the room one transfer holds in the quota. It grows as bytes are sealed.
type Reservation struct {
	s          *Store
	id         int64
	owner      string
	reserved   int64
	used       int64
	sinceCheck int64
	released   bool
}

// ID is the transfer id.
func (r *Reservation) ID() int64 { return r.id }

// Reserved is the room held now.
func (r *Reservation) Reserved() int64 {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	return r.reserved
}

// Used is the number of bytes sealed so far, on the disk.
func (r *Reservation) Used() int64 {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	return r.used
}

// Reserve takes n bytes of the quota for transfer id, owned by owner (the role's name or id; only equality is used). It refuses with a
// *QuotaError when the sum would pass the quota and with a *DiskError when the free space of the file system would fall below the
// reserve once every reservation is written.
func (s *Store) Reserve(id int64, owner string, n int64) (*Reservation, error) {
	if n < 0 {
		return nil, fmt.Errorf("staging: negative reservation")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.res[id]; ok {
		return nil, ErrExists
	}
	sp, err := s.fs.Space(s.dir)
	if err != nil {
		return nil, fmt.Errorf("staging: %w", err)
	}
	quota := s.cfg.Quota
	if quota <= 0 {
		quota = DefaultQuota(sp.Total)
	}
	if sum := s.sumLocked(); sum+n > quota {
		return nil, s.quotaErrLocked(owner, quota, sum, n)
	}
	if derr := s.diskCheckLocked(sp, n); derr != nil {
		return nil, derr
	}
	r := &Reservation{s: s, id: id, owner: owner, reserved: n}
	s.res[id] = r
	return r, nil
}

// Adopt rebuilds a reservation after a restart from what the record says. It does not refuse for quota (a lowered quota must not make
// existing files unreachable); the next Reserve sees the sum.
func (s *Store) Adopt(id int64, owner string, reserved, used int64) (*Reservation, error) {
	if reserved < used {
		reserved = used
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.res[id]; ok {
		return nil, ErrExists
	}
	r := &Reservation{s: s, id: id, owner: owner, reserved: reserved, used: used}
	s.res[id] = r
	return r, nil
}

func (s *Store) sumLocked() int64 {
	var sum int64
	for _, r := range s.res {
		sum += r.reserved
	}
	return sum
}

func (s *Store) unwrittenLocked() int64 {
	var u int64
	for _, r := range s.res {
		if r.reserved > r.used {
			u += r.reserved - r.used
		}
	}
	return u
}

func (s *Store) quotaErrLocked(owner string, quota, sum, req int64) *QuotaError {
	e := &QuotaError{Quota: quota, Reserved: sum, Requested: req}
	for _, r := range s.res {
		if r.owner == owner {
			e.Own = append(e.Own, Hold{ID: r.id, Bytes: r.reserved})
		} else {
			e.Others++
		}
	}
	sort.Slice(e.Own, func(i, j int) bool { return e.Own[i].ID < e.Own[j].ID })
	return e
}

// diskCheckLocked refuses when, with every reservation written and extra more, less than the reserve would be free.
func (s *Store) diskCheckLocked(sp Space, extra int64) error {
	reserve := s.cfg.Reserve(sp.Total)
	need := s.unwrittenLocked() + extra
	if need < 0 || uint64(need) > sp.Free || sp.Free-uint64(need) < reserve {
		return &DiskError{Free: sp.Free, Needed: need, Reserve: reserve}
	}
	return nil
}

// Use records that n bytes are about to be sealed. The reservation grows when it has to; if the quota does not allow the growth, the
// call fails with a *QuotaError and changes nothing, so the writer stops and the other reservations are untouched. Every CheckEvery bytes
// the free space of the file system is looked at, and the call fails with a *DiskError when the reserve is crossed.
func (r *Reservation) Use(n int64) error {
	s := r.s
	s.mu.Lock()
	defer s.mu.Unlock()
	if r.released {
		return ErrClosed
	}
	newUsed := r.used + n
	grow := int64(0)
	if newUsed > r.reserved {
		grow = newUsed - r.reserved
		sum := s.sumLocked()
		quota := s.cfg.Quota
		if quota <= 0 {
			if sp, err := s.fs.Space(s.dir); err == nil {
				quota = DefaultQuota(sp.Total)
			}
		}
		if sum+grow > quota {
			return s.quotaErrLocked(r.owner, quota, sum, grow)
		}
	}
	if r.sinceCheck+n >= s.cfg.CheckEvery {
		sp, err := s.fs.Space(s.dir)
		if err != nil {
			return fmt.Errorf("staging: %w", err)
		}
		// The bytes inside the room are in the unwritten sum already; the growth is not, so it is added.
		if derr := s.diskCheckLocked(sp, grow); derr != nil {
			return derr
		}
		r.sinceCheck = 0
	} else {
		r.sinceCheck += n
	}
	r.reserved += grow
	r.used = newUsed
	return nil
}

// Settle ends the growth of a finished file: the room held becomes the bytes used, so a generous estimate gives the rest back.
func (r *Reservation) Settle() {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	if !r.released && r.reserved > r.used {
		r.reserved = r.used
	}
}

// Release gives all the room back. It is idempotent.
func (r *Reservation) Release() {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	if r.released {
		return
	}
	r.released = true
	if cur, ok := r.s.res[r.id]; ok && cur == r {
		delete(r.s.res, r.id)
	}
}

// SetQuota changes the quota of the sum of the reservations (zero: the default of the file system). The cluster setting
// stagingQuotaBytes can change while kingresd runs; reservations that exist stay, the next Reserve and the next growth see the new value.
func (s *Store) SetQuota(n int64) {
	s.mu.Lock()
	s.cfg.Quota = n
	s.mu.Unlock()
}
