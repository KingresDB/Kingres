// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

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
//     with status 2, unless the caller says that it gave that device on purpose
//     (NoOutputEnv, L14-01: Python's subprocess.DEVNULL, daemon(3) and the
//     double-fork recipes give exactly that, and the two cases cannot be told apart
//     by the descriptor).
//
// Windows needs nothing: there a closed standard output is an invalid handle, and
// the write fails and gives status 2 like any other failed write.
package stdio

import "os"

// Substituted reports whether f, the standard output, is the null device that
// the Go runtime puts in place of a descriptor that was closed at start. It is
// false on a system where that cannot be told.
func Substituted(f *os.File) bool { return substituted(f) }

// NoOutputEnv is the environment variable with which a caller says that the null
// standard output it gave is what it wants (L14-01): "I gave you a null standard
// output on purpose, the status is the only answer I read". Python's
// subprocess.DEVNULL, daemon(3) and the double-fork recipes open the null device
// for reading and writing, which cannot be told from the one the runtime opens in
// place of a closed descriptor; the caller says which it is. Only the value 1
// counts.
const NoOutputEnv = "KINGRES_VERIFY_NO_OUTPUT"

// Refused reports whether the program must not run because its standard output
// f looks closed (Substituted) and the caller did not say, with NoOutputEnv set
// to 1 in getenv, that it is the null device on purpose. The variable changes
// nothing when the standard output is anything else.
func Refused(f *os.File, getenv func(string) string) bool {
	return Substituted(f) && getenv(NoOutputEnv) != "1"
}
