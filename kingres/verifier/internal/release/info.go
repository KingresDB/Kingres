// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026 RooyeKhat Media Co

package release

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// InfoName is the name of the file of a release that says which release it is. It is listed in SHA256SUMS like a binary, so the
// signed note covers it: the version, the commit and the date are signed with the hashes (P7R-07).
const InfoName = "RELEASE"

// MaxInfo is the largest RELEASE file accepted.
const MaxInfo = 1024

// Info is the content of the RELEASE file.
type Info struct {
	Version string
	Commit  string // 40 lower case hex characters, or "unknown" for a build that was not made from a git checkout
	Date    string // the commit time in UTC, YYYY-MM-DDTHH:MM:SSZ, or "unknown"
	Go      string // the Go release that built the files
}

var (
	infoVersionRE = regexp.MustCompile(`^[0-9A-Za-z][0-9A-Za-z._+-]{0,63}$`)
	infoCommitRE  = regexp.MustCompile(`^([0-9a-f]{40}|unknown)$`)
	infoDateRE    = regexp.MustCompile(`^([0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z|unknown)$`)
	infoGoRE      = regexp.MustCompile(`^go[0-9][0-9A-Za-z.+-]{0,31}$`)
)

const infoHeader = "kingres-verify release"

// FormatInfo writes the RELEASE text: a header line and four "key: value" lines, in this order.
func FormatInfo(i Info) ([]byte, error) {
	if err := i.check(); err != nil {
		return nil, err
	}
	return []byte(fmt.Sprintf("%s\nversion: %s\ncommit: %s\ndate: %s\ngo: %s\n", infoHeader, i.Version, i.Commit, i.Date, i.Go)), nil
}

func (i Info) check() error {
	switch {
	case !infoVersionRE.MatchString(i.Version):
		return errors.New("the version is not a plain version string")
	case !infoCommitRE.MatchString(i.Commit):
		return errors.New("the commit is not 40 lower case hex characters or \"unknown\"")
	case !infoDateRE.MatchString(i.Date):
		return errors.New("the date is not YYYY-MM-DDTHH:MM:SSZ or \"unknown\"")
	case !infoGoRE.MatchString(i.Go):
		return errors.New("the Go release is not of the form go1.2.3")
	}
	return nil
}

// ParseInfo reads the RELEASE text strictly: exactly the five lines FormatInfo writes, nothing else.
func ParseInfo(data []byte) (Info, error) {
	var i Info
	if len(data) > MaxInfo {
		return i, errors.New("the RELEASE file is larger than 1 KiB")
	}
	text := string(data)
	if !strings.HasSuffix(text, "\n") {
		return i, errors.New("the RELEASE file does not end with a newline")
	}
	lines := strings.Split(strings.TrimSuffix(text, "\n"), "\n")
	if len(lines) != 5 || lines[0] != infoHeader {
		return i, errors.New("the RELEASE file is not the header line and the four lines version, commit, date, go")
	}
	for n, key := range []string{"version", "commit", "date", "go"} {
		v, ok := strings.CutPrefix(lines[n+1], key+": ")
		if !ok {
			return i, fmt.Errorf("line %d of the RELEASE file is not %q", n+2, key+": VALUE")
		}
		switch key {
		case "version":
			i.Version = v
		case "commit":
			i.Commit = v
		case "date":
			i.Date = v
		case "go":
			i.Go = v
		}
	}
	return i, i.check()
}
