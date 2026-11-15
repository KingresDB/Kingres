// SPDX-License-Identifier: AGPL-3.0-only

// Package pgconfig implements phase 13 of KingresDB (design config-phase13.md): the PostgreSQL
// parameters of the servers, set at the cluster, server group, server and database level from the
// console. This file is the validation of a value against what PostgreSQL itself reports about the
// parameter (pg_settings); policy.go decides which parameters may be set where; agent.go is the
// part that runs on every server; leader.go is the manager leader's side.
package pgconfig

import (
	"errors"
	"fmt"
	"math/big"
	"regexp"
	"strings"
	"unicode/utf8"
)

// Setting is what pg_settings says about one parameter.
type Setting struct {
	Name      string   `json:"name"`
	Category  string   `json:"category"`
	ShortDesc string   `json:"shortDesc"`
	VarType   string   `json:"vartype"`
	Unit      string   `json:"unit,omitempty"`
	MinVal    string   `json:"minVal,omitempty"`
	MaxVal    string   `json:"maxVal,omitempty"`
	EnumVals  []string `json:"enumVals,omitempty"`
	Context   string   `json:"context"`
	BootVal   string   `json:"bootVal,omitempty"`
	// Setting is the running value (pg_settings.setting, in the base unit).
	Setting        string `json:"setting"`
	PendingRestart bool   `json:"pendingRestart,omitempty"`
	Source         string `json:"source,omitempty"`
	SourceFile     string `json:"sourceFile,omitempty"`
	// PendingValue is the value the override file holds while a restart is pending.
	PendingValue string `json:"pendingValue,omitempty"`
	// Error is PostgreSQL's complaint about this parameter in the override file.
	Error string `json:"error,omitempty"`
}

// ErrInvalid is wrapped by every validation error; the message is for the operator.
var ErrInvalid = errors.New("invalid value")

func bad(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, a...))
}

// MaxValueBytes is the longest value.
const MaxValueBytes = 4096

// Result is a value that passed Check.
type Result struct {
	// Text is what is stored and written: booleans are on/off, enums have PostgreSQL's spelling, other
	// values are the text the operator typed, trimmed.
	Text string
	// Base is the value in the parameter's base unit (pg_settings.unit) for integers; BaseOK says it is set.
	Base   *big.Rat
	BaseOK bool
}

var numberRE = regexp.MustCompile(`^([+-]?)(0|[1-9][0-9]*)(\.[0-9]+)?[ ]?([A-Za-z]+)?$`)
var realRE = regexp.MustCompile(`^([+-]?)((?:0|[1-9][0-9]*)(?:\.[0-9]+)?|\.[0-9]+)[ ]?([A-Za-z]+)?$`)

// Unit multipliers in bytes and in milliseconds. PostgreSQL accepts exactly these spellings.
var (
	sizeUnits = map[string]int64{"B": 1, "kB": 1 << 10, "MB": 1 << 20, "GB": 1 << 30, "TB": 1 << 40}
	timeUnits = map[string]*big.Rat{
		"us": big.NewRat(1, 1000), "ms": big.NewRat(1, 1), "s": big.NewRat(1000, 1), "min": big.NewRat(60000, 1),
		"h": big.NewRat(3600000, 1), "d": big.NewRat(86400000, 1),
	}
)

// baseUnit returns what one unit of the pg_settings unit is: family "size" with bytes, or "time" with ms.
func baseUnit(unit string) (family string, mult *big.Rat, ok bool) {
	switch unit {
	case "":
		return "", nil, false
	case "B", "kB", "MB", "GB", "TB":
		return "size", big.NewRat(sizeUnits[unit], 1), true
	case "8kB":
		return "size", big.NewRat(8192, 1), true
	case "us", "ms", "s", "min", "h", "d":
		return "time", timeUnits[unit], true
	}
	return "", nil, false
}

// parseNumber turns "1.5 MB" into the value in the parameter's base unit. integer says the result
// must be a whole number of base units (PostgreSQL would round; kingresd refuses instead).
func parseNumber(s Setting, v string, re *regexp.Regexp, integer bool) (*big.Rat, error) {
	m := re.FindStringSubmatch(v)
	if m == nil {
		return nil, bad("%q is not a decimal number; write it in decimal, without a leading zero, hexadecimal, octal or exponent form", trunc(v))
	}
	var suffix, intPart, frac string
	sign := m[1]
	if re == numberRE {
		intPart, frac, suffix = m[2], m[3], m[4]
	} else {
		intPart, suffix = m[2], m[3]
	}
	num, ok := new(big.Rat).SetString(sign + intPart + frac)
	if !ok {
		return nil, bad("%q is not a number", trunc(v))
	}
	family, mult, hasUnit := baseUnit(s.Unit)
	if suffix == "" {
		if frac != "" && integer {
			return nil, bad("%q has a fraction but no unit; PostgreSQL would round it, so kingresd refuses it", trunc(v))
		}
		return num, nil
	}
	if !hasUnit {
		return nil, bad("this setting has no unit, but %q has one (%s)", trunc(v), suffix)
	}
	var factor *big.Rat
	switch family {
	case "size":
		f, ok := sizeUnits[suffix]
		if !ok {
			return nil, bad("%q is not a size unit; use B, kB, MB, GB or TB", suffix)
		}
		factor = big.NewRat(f, 1)
	case "time":
		f, ok := timeUnits[suffix]
		if !ok {
			return nil, bad("%q is not a time unit; use us, ms, s, min, h or d", suffix)
		}
		factor = f
	}
	base := new(big.Rat).Mul(num, factor)
	base.Quo(base, mult)
	if integer && !base.IsInt() {
		return nil, bad("%q is not a whole number of %s", trunc(v), s.Unit)
	}
	return base, nil
}

func trunc(s string) string {
	if len(s) > 40 {
		return s[:40] + "..."
	}
	return s
}

func ratOf(s string) (*big.Rat, bool) {
	if s == "" {
		return nil, false
	}
	r, ok := new(big.Rat).SetString(s)
	return r, ok
}

func unitWord(u string) string {
	if u == "" {
		return ""
	}
	return " " + u
}

// Check validates a value for the parameter and returns what to store. It implements a narrow,
// unambiguous subset of PostgreSQL's syntax (design config-phase13.md, V8): what it accepts means
// the same to PostgreSQL, and a differential test against a real instance keeps it so.
func Check(s Setting, value string) (Result, error) {
	v := strings.TrimSpace(value)
	if !utf8.ValidString(v) || strings.ContainsAny(v, "\x00\r\n") {
		return Result{}, bad("the value contains a newline, NUL or invalid UTF-8")
	}
	if len(v) > MaxValueBytes {
		return Result{}, bad("the value is longer than %d bytes", MaxValueBytes)
	}
	switch s.VarType {
	case "bool":
		switch strings.ToLower(v) {
		case "on", "true", "yes", "1":
			return Result{Text: "on"}, nil
		case "off", "false", "no", "0":
			return Result{Text: "off"}, nil
		}
		return Result{}, bad("%q is not a boolean; use on or off", trunc(v))
	case "integer", "real":
		re, integer := numberRE, true
		if s.VarType == "real" {
			re, integer = realRE, false
		}
		n, err := parseNumber(s, v, re, integer)
		if err != nil {
			return Result{}, err
		}
		if lo, ok := ratOf(s.MinVal); ok && n.Cmp(lo) < 0 {
			return Result{}, bad("%s is below the smallest value, %s%s", trunc(v), s.MinVal, unitWord(s.Unit))
		}
		if hi, ok := ratOf(s.MaxVal); ok && n.Cmp(hi) > 0 {
			return Result{}, bad("%s is above the largest value, %s%s", trunc(v), s.MaxVal, unitWord(s.Unit))
		}
		return Result{Text: v, Base: n, BaseOK: true}, nil
	case "enum":
		for _, e := range s.EnumVals {
			if strings.EqualFold(e, v) {
				return Result{Text: e}, nil
			}
		}
		// PostgreSQL accepts the spellings of a boolean for an enum that has on and off.
		if has(s.EnumVals, "on") && has(s.EnumVals, "off") {
			switch strings.ToLower(v) {
			case "true", "yes", "1":
				return Result{Text: "on"}, nil
			case "false", "no", "0":
				return Result{Text: "off"}, nil
			}
		}
		return Result{}, bad("%q is not one of: %s", trunc(v), strings.Join(s.EnumVals, ", "))
	case "string":
		return Result{Text: v}, nil
	}
	return Result{}, bad("the parameter has the unknown type %q", s.VarType)
}

func has(l []string, x string) bool {
	for _, e := range l {
		if e == x {
			return true
		}
	}
	return false
}

// Format shows a pg_settings.setting (a number in the base unit) the way SHOW does: the largest unit
// that divides it evenly, "128MB", "10min". Other types are returned as they are.
func Format(s Setting, setting string) string {
	if s.VarType != "integer" && s.VarType != "real" || s.Unit == "" {
		return setting
	}
	n, ok := new(big.Int).SetString(setting, 10)
	if !ok {
		return setting + s.Unit
	}
	family, mult, ok := baseUnit(s.Unit)
	if !ok || !mult.IsInt() {
		return setting + s.Unit
	}
	if n.Sign() < 0 {
		return setting // -1 means "off" and has no unit in SHOW
	}
	if n.Sign() == 0 {
		return "0"
	}
	switch family {
	case "size":
		bytes := new(big.Int).Mul(n, mult.Num())
		for _, u := range []struct {
			n string
			m int64
		}{{"TB", 1 << 40}, {"GB", 1 << 30}, {"MB", 1 << 20}, {"kB", 1 << 10}} {
			if new(big.Int).Mod(bytes, big.NewInt(u.m)).Sign() == 0 {
				return new(big.Int).Div(bytes, big.NewInt(u.m)).String() + u.n
			}
		}
		return bytes.String() + "B"
	default:
		ms := new(big.Rat).Mul(new(big.Rat).SetInt(n), mult)
		if !ms.IsInt() {
			return setting + s.Unit
		}
		msi := ms.Num()
		for _, u := range []struct {
			n string
			m int64
		}{{"d", 86400000}, {"h", 3600000}, {"min", 60000}, {"s", 1000}} {
			if new(big.Int).Mod(msi, big.NewInt(u.m)).Sign() == 0 {
				return new(big.Int).Div(msi, big.NewInt(u.m)).String() + u.n
			}
		}
		return msi.String() + "ms"
	}
}
