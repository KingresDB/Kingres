// SPDX-License-Identifier: AGPL-3.0-only

//go:build unix

package statefile

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// What CheckPrivate looks at, and why. The state is the auditor's protection
// against a rollback. Another local account that can change anything the state
// is reached through can swap the whole record for an older one, or point the
// name at an empty directory, and the run that follows ends with status 0. So
// the check covers everything such an account could redirect, as OpenSSH does
// for the files it reads (StrictModes): every component of the path, from the
// root, as the path is resolved, links included.
//
//   - every directory the path passes through, root included, is owned by root
//     or by the user running this command and is not writable by group or others,
//     unless it has the sticky bit (/tmp: an entry that is not theirs cannot be
//     renamed or removed by others);
//   - the directory that holds the state (and every directory that holds a link
//     the path goes through) is stricter: owned by the user running this command,
//     and not writable by group or others even with the sticky bit, because
//     anybody who can create a name there can plant the lock or the evidence of a
//     fork;
//   - every link on the way is owned by root or by the user running this command
//     (the directory that holds it is checked as above), and is followed with the
//     same checks for its target;
//   - the state itself, when it exists, is owned by the user running this command
//     and not writable by group or others.
//
// What is not checked: access control lists and extended attributes (only the
// permission bits and the owner are read), root and anybody who is root, a file
// system that does not keep owners and modes (a network or FAT volume mounted
// with fixed permissions), and a path changed between this check and the open by
// somebody the check already trusts. A directory that nobody but the owner and
// root can write cannot change under a run, which is why the check is a check
// and not a race.

// ownerOf returns the user id that owns a file, and false when the system does
// not say. Tests replace it to put another owner on a path.
var ownerOf = func(_ string, st fs.FileInfo) (int, bool) {
	if sys, ok := st.Sys().(*syscall.Stat_t); ok {
		return int(sys.Uid), true
	}
	return 0, false
}

// selfUID is the user running this command; tests replace it.
var selfUID = os.Geteuid

const maxLinks = 40

// CheckPrivate checks the path the state was named by (as typed: a relative
// path is taken from the working directory) and everything it passes through.
// The error says which component fails and why.
func CheckPrivate(typed string) error {
	if !filepath.IsAbs(typed) {
		wd, err := os.Getwd()
		if err != nil {
			return err
		}
		typed = filepath.Join(wd, typed)
	}
	w := &walker{}
	return w.walk(typed)
}

type walker struct{ links int }

func components(p string) []string {
	var out []string
	for _, c := range strings.Split(p, string(filepath.Separator)) {
		if c != "" {
			out = append(out, c)
		}
	}
	return out
}

func (w *walker) walk(p string) error {
	cur := string(filepath.Separator)
	if err := checkDir(cur, false); err != nil {
		return err
	}
	queue := components(p)
	for len(queue) > 0 {
		c := queue[0]
		queue = queue[1:]
		last := len(queue) == 0
		switch c {
		case ".":
			continue
		case "..":
			cur = filepath.Dir(cur)
			continue
		}
		if last {
			// cur holds the final component: the state, or a link that leads to it.
			if err := checkDir(cur, true); err != nil {
				return err
			}
		}
		next := filepath.Join(cur, c)
		st, err := os.Lstat(next)
		if errors.Is(err, fs.ErrNotExist) && last {
			return nil // a state that does not exist yet
		}
		if err != nil {
			return err
		}
		switch {
		case st.Mode()&fs.ModeSymlink != 0:
			if w.links++; w.links > maxLinks {
				return fmt.Errorf("%s: too many links on the way", next)
			}
			if uid, ok := ownerOf(next, st); ok && uid != 0 && uid != selfUID() {
				return fmt.Errorf("the link %s is owned by user %d, not by root or the user running this command (%d)", next, uid, selfUID())
			}
			target, err := os.Readlink(next)
			if err != nil {
				return err
			}
			if filepath.IsAbs(target) {
				cur = string(filepath.Separator)
			}
			queue = append(components(target), queue...)
		case st.IsDir():
			if err := checkDir(next, last); err != nil {
				return err
			}
			cur = next
		default:
			if !last {
				return fmt.Errorf("%s is not a directory", next)
			}
			return checkFile(next, st, "the file")
		}
	}
	return nil
}

// checkDir checks one directory the path passes through; holder says it is the
// directory that holds the final component (or a link), which is held to the
// stricter rule.
func checkDir(p string, holder bool) error {
	st, err := os.Stat(p)
	if err != nil {
		return err
	}
	uid, known := ownerOf(p, st)
	self := selfUID()
	m := st.Mode()
	what := "the directory"
	if holder {
		what = "the directory that holds the state"
	}
	if known {
		if holder && uid != self {
			return fmt.Errorf("%s %s is owned by user %d, not by the user running this command (%d)", what, p, uid, self)
		}
		if uid != 0 && uid != self {
			return fmt.Errorf("%s %s is owned by user %d, not by root or the user running this command (%d)", what, p, uid, self)
		}
	}
	if m.Perm()&0o022 != 0 && (holder || m&fs.ModeSticky == 0) {
		return fmt.Errorf("%s %s can be written by group or others (mode %04o)", what, p, m.Perm())
	}
	return nil
}

// checkFile checks the state itself.
func checkFile(p string, st fs.FileInfo, what string) error {
	if uid, ok := ownerOf(p, st); ok && uid != selfUID() {
		return fmt.Errorf("%s %s is owned by user %d, not by the user running this command (%d)", what, p, uid, selfUID())
	}
	if m := st.Mode().Perm(); m&0o022 != 0 {
		return fmt.Errorf("%s %s can be written by group or others (mode %04o)", what, p, m)
	}
	return nil
}
