// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package worm

import (
	"encoding/json"
	"testing"
)

func FuzzParseS3Error(f *testing.F) {
	for _, s := range []string{
		`<Error><Code>NoSuchKey</Code><Message>m</Message></Error>`,
		`<?xml version="1.0"?><Error><Code>X</Code></Error>`,
		`<Error>`, `</Error>`, `<a><b></a>`, `<!DOCTYPE x [<!ENTITY a "b">]><Error><Code>&a;</Code></Error>`,
		`<Error><Code><![CDATA[x]]></Code></Error>`, "", "\x00\xff",
	} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		code, msg := ParseS3Error(b)
		if len(code) > 64 || len(msg) > MaxMessage {
			t.Fatalf("limits exceeded: %d %d", len(code), len(msg))
		}
		for i := 0; i < len(msg); i++ {
			if msg[i] < 0x20 || msg[i] > 0x7e {
				t.Fatalf("non printable byte in %q", msg)
			}
		}
		if code != "" && !codeRE.MatchString(code) {
			t.Fatalf("bad code %q", code)
		}
	})
}

func FuzzParseConfig(f *testing.F) {
	f.Add([]byte(`{"region":"r1","bucket":"my-bucket"}`))
	f.Add([]byte(`{"region":"r1","bucket":"my-bucket","endpoint":"https://h:1","mode":"compliance","retentionDays":5}`))
	f.Add([]byte(`null`))
	f.Fuzz(func(t *testing.T, b []byte) {
		c, err := ParseConfig(json.RawMessage(b))
		if err != nil {
			return
		}
		// a config that parses is stable under normalisation
		c2, err := c.Normalize()
		if err != nil || c2.Endpoint != c.Endpoint || c2.KeyPrefix != c.KeyPrefix || c2.Mode != c.Mode {
			t.Fatalf("not stable: %+v %v", c, err)
		}
		if _, err := New(Options{Config: c, AccessKeyID: "a", SecretAccessKey: "b"}); err != nil {
			t.Fatal(err)
		}
	})
}
