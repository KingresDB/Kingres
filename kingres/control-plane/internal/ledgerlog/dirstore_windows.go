// SPDX-License-Identifier: AGPL-3.0-only

package ledgerlog

import (
	"errors"
	"syscall"
)

// platformRenameBusy reports whether a failed rename is the kind that another
// program's open handle causes: access denied, a sharing violation or a lock
// violation. Waiting can cure it.
func platformRenameBusy(err error) bool {
	var errno syscall.Errno
	if errors.As(err, &errno) {
		switch errno {
		case 5, 32, 33: // ERROR_ACCESS_DENIED, ERROR_SHARING_VIOLATION, ERROR_LOCK_VIOLATION
			return true
		}
	}
	return false
}
