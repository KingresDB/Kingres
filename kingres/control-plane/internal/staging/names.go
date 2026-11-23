// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package staging

import (
	"regexp"
	"strconv"
)

// MarkerName is the file that marks a directory as staging space.
const MarkerName = ".kingres-transfer"

const markerText = "kingres-transfer-dir 1\n"

// namePattern is the only form of name the janitor may delete. It is the design's ^transfer-[0-9]+\.(part|kdx)$ with the number
// limited to 18 digits, so that it always fits an int64; a longer name is foreign and is never touched.
var namePattern = regexp.MustCompile(`^transfer-([0-9]{1,18})\.(part|kdx)$`)

// PartName is the name of the file while it is written.
func PartName(id int64) string { return "transfer-" + strconv.FormatInt(id, 10) + ".part" }

// FinalName is the name of the complete file.
func FinalName(id int64) string { return "transfer-" + strconv.FormatInt(id, 10) + ".kdx" }

// ParseName reads a file name of the store. ok is false for every other name.
func ParseName(name string) (id int64, final bool, ok bool) {
	m := namePattern.FindStringSubmatch(name)
	if m == nil {
		return 0, false, false
	}
	n, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil {
		return 0, false, false
	}
	return n, m[2] == "kdx", true
}
