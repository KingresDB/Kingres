// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026 RooyeKhat Media Co

package release

import (
	"strings"
	"testing"
)

func TestInfoRoundTripAndStrictness(t *testing.T) {
	in := Info{Version: "0.1.0", Commit: strings.Repeat("ab", 20), Date: "2026-11-21T10:00:00Z", Go: "go1.27.3"}
	b, err := FormatInfo(in)
	if err != nil {
		t.Fatal(err)
	}
	out, err := ParseInfo(b)
	if err != nil || out != in {
		t.Fatalf("%v %+v", err, out)
	}
	if _, err := FormatInfo(Info{Version: "0.1.0", Commit: "unknown", Date: "unknown", Go: "go1.27.3"}); err != nil {
		t.Fatalf("a build that is not from git: %v", err)
	}
	good := string(b)
	for name, bad := range map[string]string{
		"no newline":   strings.TrimSuffix(good, "\n"),
		"extra line":   good + "extra: x\n",
		"order":        strings.Replace(strings.Replace(good, "version: 0.1.0\ncommit", "commit", 1), "go: go1.27.3\n", "go: go1.27.3\nversion: 0.1.0\n", 1),
		"upper commit": strings.Replace(good, "ab", "AB", 1),
		"short commit": strings.Replace(good, strings.Repeat("ab", 20), "abab", 1),
		"bad date":     strings.Replace(good, "2026-11-21T10:00:00Z", "yesterday", 1),
		"bad version":  strings.Replace(good, "0.1.0", "-0.1.0", 1),
		"control byte": strings.Replace(good, "0.1.0", "0.1.\x1b0", 1),
		"no header":    strings.Replace(good, "kingres-verify release\n", "", 1),
		"big":          good + strings.Repeat("x", MaxInfo),
		"carriage":     strings.Replace(good, "\n", "\r\n", 1),
	} {
		if _, err := ParseInfo([]byte(bad)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
