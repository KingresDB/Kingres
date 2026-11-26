// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package pgconfig

import (
	"errors"
	"strings"
	"testing"
)

var (
	workMem    = Setting{Name: "work_mem", VarType: "integer", Unit: "kB", MinVal: "64", MaxVal: "2147483647", Context: "user"}
	sharedBuf  = Setting{Name: "shared_buffers", VarType: "integer", Unit: "8kB", MinVal: "16", MaxVal: "1073741823", Context: "postmaster"}
	stmtTO     = Setting{Name: "statement_timeout", VarType: "integer", Unit: "ms", MinVal: "0", MaxVal: "2147483647", Context: "user"}
	naptime    = Setting{Name: "autovacuum_naptime", VarType: "integer", Unit: "s", MinVal: "1", MaxVal: "2147483", Context: "sighup"}
	maxConn    = Setting{Name: "max_connections", VarType: "integer", MinVal: "1", MaxVal: "262143", Context: "postmaster"}
	rpc        = Setting{Name: "random_page_cost", VarType: "real", MinVal: "0", MaxVal: "1.79769e+308", Context: "user"}
	seqscan    = Setting{Name: "enable_seqscan", VarType: "bool", Context: "user"}
	walLevel   = Setting{Name: "wal_level", VarType: "enum", EnumVals: []string{"minimal", "replica", "logical"}, Context: "postmaster"}
	syncCommit = Setting{Name: "synchronous_commit", VarType: "enum", EnumVals: []string{"local", "remote_write", "remote_apply", "on", "off"}, Context: "user"}
	timeZone   = Setting{Name: "TimeZone", VarType: "string", Context: "user"}
)

func TestCheck(t *testing.T) {
	type tc struct {
		s    Setting
		in   string
		ok   bool
		text string
		base string // expected base value, "" when not checked
	}
	cases := []tc{
		{workMem, "64MB", true, "64MB", "65536"},
		{workMem, "64 MB", true, "64 MB", "65536"},
		{workMem, "1.5MB", true, "1.5MB", "1536"},
		{workMem, "4096", true, "4096", "4096"},
		{workMem, "64kB", true, "64kB", "64"},
		{workMem, "63kB", false, "", ""},
		{workMem, "1GB", true, "1GB", "1048576"},
		{workMem, "3000000000kB", false, "", ""},
		{workMem, "0x10", false, "", ""},
		{workMem, "010", false, "", ""},
		{maxConn, "0100", false, "", ""}, // PostgreSQL reads it as octal 64: the range check does not shadow the leading zero
		{maxConn, "00", false, "", ""},
		{maxConn, "007", false, "", ""},
		{workMem, "1e3", false, "", ""},
		{workMem, "1.5", false, "", ""}, // fraction without a unit: PostgreSQL would round
		{workMem, "1.0001kB", false, "", ""},
		{workMem, "64mb", false, "", ""},
		{workMem, "64ms", false, "", ""},
		{workMem, "", false, "", ""},
		{workMem, " 128MB ", true, "128MB", "131072"},
		{workMem, "-1", false, "", ""},
		{workMem, "abc", false, "", ""},
		{workMem, "99999999999999999999999999kB", false, "", ""},
		{sharedBuf, "128MB", true, "128MB", "16384"},
		{sharedBuf, "16384", true, "16384", "16384"},
		{sharedBuf, "100kB", false, "", ""}, // not a whole number of 8kB blocks
		{sharedBuf, "8kB", false, "", ""},   // below the minimum of 16 blocks
		{stmtTO, "5s", true, "5s", "5000"},
		{stmtTO, "1min", true, "1min", "60000"},
		{stmtTO, "0", true, "0", "0"},
		{stmtTO, "500us", false, "", ""},
		{stmtTO, "1d", true, "1d", "86400000"},
		{stmtTO, "5MB", false, "", ""},
		{naptime, "1min", true, "1min", "60"},
		{naptime, "500ms", false, "", ""},
		{maxConn, "200", true, "200", "200"},
		{maxConn, "200MB", false, "", ""},
		{maxConn, "0", false, "", ""},
		{maxConn, "262144", false, "", ""},
		{maxConn, "+5", true, "+5", "5"},
		{rpc, "1.1", true, "1.1", ""},
		{rpc, ".5", true, ".5", ""},
		{rpc, "-1", false, "", ""},
		{rpc, "1e2", false, "", ""},
		{rpc, "x", false, "", ""},
		{seqscan, "OFF", true, "off", ""},
		{seqscan, "yes", true, "on", ""},
		{seqscan, "1", true, "on", ""},
		{seqscan, "t", false, "", ""}, // PostgreSQL accepts prefixes; kingresd does not
		{seqscan, "o", false, "", ""},
		{walLevel, "LOGICAL", true, "logical", ""},
		{walLevel, "bogus", false, "", ""},
		{syncCommit, "true", true, "on", ""},
		{syncCommit, "Remote_Apply", true, "remote_apply", ""},
		{timeZone, "Asia/Tehran", true, "Asia/Tehran", ""},
		{timeZone, "a\nb", false, "", ""},
		{timeZone, "a\x00b", false, "", ""},
		{timeZone, "\xff", false, "", ""},
		{timeZone, strings.Repeat("x", MaxValueBytes+1), false, "", ""},
		{timeZone, strings.Repeat("x", MaxValueBytes), true, strings.Repeat("x", MaxValueBytes), ""},
		{Setting{Name: "x", VarType: "mystery"}, "1", false, "", ""},
	}
	for _, c := range cases {
		r, err := Check(c.s, c.in)
		if (err == nil) != c.ok {
			t.Errorf("%s=%q: ok=%v err=%v", c.s.Name, trunc(c.in), c.ok, err)
			continue
		}
		if err != nil {
			if !errors.Is(err, ErrInvalid) {
				t.Errorf("%s=%q: error %v does not wrap ErrInvalid", c.s.Name, c.in, err)
			}
			continue
		}
		if r.Text != c.text {
			t.Errorf("%s=%q: text %q, want %q", c.s.Name, trunc(c.in), trunc(r.Text), trunc(c.text))
		}
		if c.base != "" {
			if !r.BaseOK || r.Base.FloatString(0) != c.base {
				t.Errorf("%s=%q: base %v, want %s", c.s.Name, c.in, r.Base, c.base)
			}
		}
	}
}

func TestFormat(t *testing.T) {
	cases := []struct {
		s    Setting
		in   string
		want string
	}{
		{sharedBuf, "16384", "128MB"}, {sharedBuf, "16", "128kB"}, {workMem, "4096", "4MB"}, {workMem, "65", "65kB"},
		{stmtTO, "0", "0"}, {stmtTO, "60000", "1min"}, {stmtTO, "1500", "1500ms"}, {stmtTO, "-1", "-1"}, {naptime, "60", "1min"},
		{naptime, "3600", "1h"}, {maxConn, "100", "100"}, {seqscan, "on", "on"}, {workMem, "1048576", "1GB"},
	}
	for _, c := range cases {
		if got := Format(c.s, c.in); got != c.want {
			t.Errorf("%s %s: %q, want %q", c.s.Name, c.in, got, c.want)
		}
	}
}
