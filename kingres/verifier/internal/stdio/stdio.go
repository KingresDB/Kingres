// SPDX-License-Identifier: AGPL-3.0-only

// Package stdio looks at the standard output before the verdict is written to
// it (L12-04).
//
// A program that is started with its standard output closed (`>&-`) does not
// see a closed descriptor in Go: the runtime opens the null device on every of
// the descriptors 0, 1 and 2 that is closed at start, and a write to it
// succeeds. A verdict written there is lost and the status would be 0. What can
// be told afterwards is how the descriptor was opened: the runtime opens the null
// device for reading and writing, and a shell that is asked for no output
// (`>/dev/null`, `>>/dev/null`) opens it for writing only. So:
//
//   - a standard output that is the null device opened for writing only is what
//     the user asked for (a caller that wants the status and not the text), and
//     the program runs as usual;
//   - a standard output that is the null device opened for reading and writing is
//     what the runtime substitutes for a closed descriptor, and it is refused
//     with status 2. A caller that really wants `1<>/dev/null` must write
//     `>/dev/null`.
//
// Windows needs nothing: there a closed standard output is an invalid handle, and
// the write fails and gives status 2 like any other failed write.
package stdio

import "os"

// Substituted reports whether f, the standard output, is the null device that
// the Go runtime puts in place of a descriptor that was closed at start. It is
// false on a system where that cannot be told.
func Substituted(f *os.File) bool { return substituted(f) }
