// SPDX-License-Identifier: AGPL-3.0-only

package task

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// FileStore is a Store that survives a restart of kingresd by writing every
// change to one JSON file (atomic replace, owner only). It holds the setup
// task, which has to exist before the system database does. It never holds
// secrets: tasks carry none.
type FileStore struct {
	inner    *MemStore
	path     string
	onChange func(Task)
	mu       sync.Mutex
}

// OpenFileStore loads path (missing means empty). onChange, when not nil, is
// called with a copy of the task after every change, outside the store's
// locks of the mutation.
func OpenFileStore(path string, onChange func(Task)) (*FileStore, error) {
	f := &FileStore{inner: NewMemStore(), path: path, onChange: onChange}
	b, err := os.ReadFile(path) //nolint:gosec // operator-owned data directory
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return nil, fmt.Errorf("task: reading %s: %w", path, err)
	default:
		var ts []Task
		if err := json.Unmarshal(b, &ts); err != nil {
			return nil, fmt.Errorf("task: parsing %s: %w", path, err)
		}
		f.inner.Import(ts)
	}
	return f, nil
}

func (f *FileStore) save(id int64) error {
	f.mu.Lock()
	b, err := json.Marshal(f.inner.Export())
	if err == nil {
		err = writeFileAtomic(f.path, b)
	}
	f.mu.Unlock()
	if err != nil {
		return fmt.Errorf("task: saving %s: %w", f.path, err)
	}
	if f.onChange != nil && id != 0 {
		if t, gerr := f.inner.Get(context.Background(), id); gerr == nil {
			f.onChange(t)
		}
	}
	return nil
}

func writeFileAtomic(path string, b []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".task-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	_, werr := tmp.Write(b)
	serr := tmp.Sync()
	cerr := tmp.Close()
	for _, e := range []error{werr, serr, cerr} {
		if e != nil {
			_ = os.Remove(name)
			return e
		}
	}
	if err := os.Rename(name, path); err != nil {
		_ = os.Remove(name)
		return err
	}
	return nil
}

// Create implements Store.
func (f *FileStore) Create(ctx context.Context, t Task) (Task, error) {
	out, err := f.inner.Create(ctx, t)
	if err != nil {
		return out, err
	}
	return out, f.save(out.ID) //nolint:contextcheck // saving does not depend on a request
}

// Get implements Store.
func (f *FileStore) Get(ctx context.Context, id int64) (Task, error) { return f.inner.Get(ctx, id) }

// Claim implements Store.
func (f *FileStore) Claim(ctx context.Context, holder string, lease time.Duration) (*Task, error) {
	t, err := f.inner.Claim(ctx, holder, lease)
	if err != nil || t == nil {
		return t, err
	}
	return t, f.save(t.ID) //nolint:contextcheck // saving does not depend on a request
}

// ClaimID implements Store.
func (f *FileStore) ClaimID(ctx context.Context, id int64, holder string, lease time.Duration) (*Task, error) {
	t, err := f.inner.ClaimID(ctx, id, holder, lease)
	if err != nil || t == nil {
		return t, err
	}
	return t, f.save(t.ID) //nolint:contextcheck // saving does not depend on a request
}

// Renew implements Store (not persisted: a restart expires the lease anyway).
func (f *FileStore) Renew(ctx context.Context, id int64, l Lease, lease time.Duration) error {
	return f.inner.Renew(ctx, id, l, lease)
}

// SetStep implements Store.
func (f *FileStore) SetStep(ctx context.Context, id int64, l Lease, i int, state string, pct float64) (bool, error) {
	c, err := f.inner.SetStep(ctx, id, l, i, state, pct)
	if err != nil {
		return c, err
	}
	return c, f.save(id) //nolint:contextcheck // saving does not depend on a request
}

// Finish implements Store.
func (f *FileStore) Finish(ctx context.Context, id int64, l Lease, state State, e *Error) error {
	if err := f.inner.Finish(ctx, id, l, state, e); err != nil {
		return err
	}
	return f.save(id) //nolint:contextcheck // saving does not depend on a request
}

// RequestCancel implements Store.
func (f *FileStore) RequestCancel(ctx context.Context, id int64, now time.Time) error {
	if err := f.inner.RequestCancel(ctx, id, now); err != nil {
		return err
	}
	return f.save(id) //nolint:contextcheck // saving does not depend on a request
}

// RequestCancelIf implements CondCancelStore.
func (f *FileStore) RequestCancelIf(ctx context.Context, id int64, now time.Time, seen State, fail *Error) error {
	if err := f.inner.RequestCancelIf(ctx, id, now, seen, fail); err != nil {
		return err
	}
	return f.save(id) //nolint:contextcheck // saving does not depend on a request
}

// List implements Store.
func (f *FileStore) List(ctx context.Context, limit int) ([]Task, error) {
	return f.inner.List(ctx, limit)
}

// Pause implements PauseStore.
func (f *FileStore) Pause(ctx context.Context, id int64, l Lease, reason string) error {
	if err := f.inner.Pause(ctx, id, l, reason); err != nil {
		return err
	}
	return f.save(id) //nolint:contextcheck // saving does not depend on a request
}

// Resume implements PauseStore.
func (f *FileStore) Resume(ctx context.Context, id int64) error {
	if err := f.inner.Resume(ctx, id); err != nil {
		return err
	}
	return f.save(id) //nolint:contextcheck // saving does not depend on a request
}

// SetMark implements MarkStore.
func (f *FileStore) SetMark(ctx context.Context, id int64, l Lease, mark string) error {
	if err := f.inner.SetMark(ctx, id, l, mark); err != nil {
		return err
	}
	return f.save(id) //nolint:contextcheck // saving does not depend on a request
}

// SetDetail implements DetailStore.
func (f *FileStore) SetDetail(ctx context.Context, id int64, l Lease, detail string) error {
	if err := f.inner.SetDetail(ctx, id, l, detail); err != nil {
		return err
	}
	return f.save(id) //nolint:contextcheck // saving does not depend on a request
}
