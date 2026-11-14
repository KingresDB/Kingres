// SPDX-License-Identifier: PostgreSQL

// Package pgconninfo is the one place in kingresd where a libpq key/value
// connection string is assembled. Every value is quoted and escaped according
// to libpq's rules, so a value that came from a request, the catalog or
// another server can never add, replace or end a keyword (the redirection of
// a service-role connection to another host is the attack this prevents).
// Nothing else in the module may format "host=...", "dbname=..." or a
// postgres:// URL; TestNoConnectionStringsAssembledElsewhere enforces it.
package pgconninfo

import (
	"errors"
	"fmt"
	"strings"
)

// Pair is one keyword and its value.
type Pair struct{ Key, Value string }

// allowed is the closed list of keywords kingresd ever sets.
var allowed = map[string]bool{
	"host": true, "port": true, "dbname": true, "user": true, "password": true,
	"sslmode": true, "sslcert": true, "sslkey": true, "sslrootcert": true, "sslcrl": true,
	"connect_timeout": true, "application_name": true, "channel_binding": true,
}

// Quote returns v as a libpq connection-string value: always in single quotes,
// with backslash and single quote escaped by a backslash. A NUL byte is an
// error (libpq cannot carry one).
func Quote(v string) (string, error) {
	if strings.IndexByte(v, 0) >= 0 {
		return "", errors.New("a connection value contains a NUL byte")
	}
	v = strings.ReplaceAll(v, `\`, `\\`)
	v = strings.ReplaceAll(v, `'`, `\'`)
	return "'" + v + "'", nil
}

// Format builds "key='value' key='value'" from the pairs, in order. A key
// outside the closed list or a value with a NUL byte is an error; the error
// never contains a value.
func Format(pairs ...Pair) (string, error) {
	parts := make([]string, 0, len(pairs))
	for _, p := range pairs {
		if !allowed[p.Key] {
			return "", fmt.Errorf("connection keyword %q is not allowed", p.Key)
		}
		q, err := Quote(p.Value)
		if err != nil {
			return "", fmt.Errorf("connection keyword %s: %w", p.Key, err)
		}
		parts = append(parts, p.Key+"="+q)
	}
	return strings.Join(parts, " "), nil
}
