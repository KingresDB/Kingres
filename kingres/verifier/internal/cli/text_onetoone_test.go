// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/rand"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"
)

// decodeUntrusted reads the output of untrusted back: the rule the README
// gives. It returns the original (the shown part, when the value was cut), the
// original length when it was cut (0 when not) and an error for anything the
// function never writes.
func decodeUntrusted(s string) (orig []byte, cutTotal int, shown int, err error) {
	var out []byte
	for i := 0; i < len(s); {
		c := s[i]
		if c != '\\' {
			out = append(out, c)
			i++
			continue
		}
		if i+1 >= len(s) {
			return nil, 0, 0, fmt.Errorf("a backslash at the end")
		}
		switch s[i+1] {
		case '\\':
			out = append(out, '\\')
			i += 2
		case 'x':
			if i+4 > len(s) {
				return nil, 0, 0, fmt.Errorf("short \\x")
			}
			n, perr := strconv.ParseUint(s[i+2:i+4], 16, 8)
			if perr != nil || s[i+2:i+4] != strings.ToLower(s[i+2:i+4]) {
				return nil, 0, 0, fmt.Errorf("bad \\x escape %q", s[i:i+4])
			}
			out = append(out, byte(n))
			i += 4
		case 'u', 'U':
			width := 4
			if s[i+1] == 'U' {
				width = 8
			}
			if i+2+width > len(s) {
				return nil, 0, 0, fmt.Errorf("short \\u")
			}
			n, perr := strconv.ParseUint(s[i+2:i+2+width], 16, 32)
			if perr != nil {
				return nil, 0, 0, fmt.Errorf("bad \\u escape")
			}
			out = utf8.AppendRune(out, rune(n))
			i += 2 + width
		case '.':
			m := regexp.MustCompile(`^\\\.\.\.\(cut: (\d+) of (\d+) bytes shown\)$`).FindStringSubmatch(s[i:])
			if m == nil {
				return nil, 0, 0, fmt.Errorf("a cut marker that is not at the end or is malformed: %q", s[i:])
			}
			a, _ := strconv.Atoi(m[1])
			b, _ := strconv.Atoi(m[2])
			return out, b, a, nil
		default:
			return nil, 0, 0, fmt.Errorf("an escape the function does not write: %q", s[i:i+2])
		}
	}
	return out, 0, 0, nil
}

func randomHostile(r *rand.Rand) string {
	pieces := []string{
		"a", "Z", " ", "~", "\\", "\\n", "\\x1b", "\\u202e", "\\...", `"`, "<", ">", "&",
		"\n", "\r", "\t", "\x00", "\x1b", "\x7f", "\x80", "\x9b", "\xff", "\xc3", "\xe2\x80",
		"\u0085", "\u00a0", "\u00e9", "\u202e", "\u202c", "\u2028", "\u2029", "\ufeff", "\u200b",
		"\u4e2d", "\U0001F600", "\U000E0001", "\U0010FFFF", "\ufffd",
	}
	var b strings.Builder
	for n := r.Intn(12); n >= 0; n-- {
		b.WriteString(pieces[r.Intn(len(pieces))])
	}
	return b.String()
}

// L9-10: the rendering is one to one. The original is recovered from the
// output, so two different inputs never print the same.
func TestUntrustedIsOneToOne(t *testing.T) {
	r := rand.New(rand.NewSource(1))
	seen := map[string]string{}
	inputs := []string{"", "\\", "\\\\", "\\n", "\n", "\\x0a", "\x0a", "a\\nb", "a\nb", "\x1b", "\\x1b", "\xff", "\\xff", "\u202e", "\\u202e",
		`C:\Users\x`, "...", "\\...(cut: 1 of 2 bytes shown)"}
	for i := 0; i < 20000; i++ {
		inputs = append(inputs, randomHostile(r))
	}
	for _, in := range inputs {
		out := untrusted(in)
		if prev, dup := seen[out]; dup && prev != in {
			t.Fatalf("%q and %q both print as %q", prev, in, out)
		}
		seen[out] = in
		got, cut, _, err := decodeUntrusted(out)
		if err != nil || cut != 0 || !bytes.Equal(got, []byte(in)) {
			t.Fatalf("%q printed as %q, which reads back as %q (cut %d, %v)", in, out, got, cut, err)
		}
		for _, c := range out {
			if c < 0x20 || c == 0x7f {
				t.Fatalf("%q printed with a raw control character: %q", in, out)
			}
		}
	}
	// The two cases of the review: a real newline and a backslash with an n.
	if untrusted("a\nb") == untrusted(`a\nb`) || untrusted("\x1b") == untrusted(`\x1b`) {
		t.Fatal("the cases of the review still print the same")
	}
}

// The cut of a long value is marked in a way no input can print, and says the
// original length.
func TestUntrustedCutIsMarkedAndCannotBeForged(t *testing.T) {
	long := strings.Repeat("\u00e9", maxUntrustedText) // 4096 bytes
	out := untrusted(long)
	got, total, shown, err := decodeUntrusted(out)
	if err != nil || total != len(long) || shown != len(got) || len(got) > maxUntrustedText || !strings.HasPrefix(long, string(got)) {
		t.Fatalf("cut value: total %d shown %d got %d bytes, %v", total, shown, len(got), err)
	}
	if !strings.HasSuffix(out, fmt.Sprintf(`\...(cut: %d of %d bytes shown)`, shown, len(long))) {
		t.Errorf("the marker: %q", out[len(out)-60:])
	}
	// An input that holds the marker text is not mistaken for a cut value.
	for _, in := range []string{`\...(cut: 3 of 99 bytes shown)`, `x \...(cut: 1 of 2 bytes shown)`} {
		if _, total, _, err := decodeUntrusted(untrusted(in)); err != nil || total != 0 {
			t.Errorf("%q: read back as a cut value (%d, %v)", in, total, err)
		}
	}
	// A cut never splits a character.
	for n := 0; n < 8; n++ {
		s := strings.Repeat("a", maxUntrustedText-n) + strings.Repeat("\U0001F600", 4)
		o := untrusted(s)
		if !utf8.ValidString(o) {
			t.Errorf("a cut inside a character gave invalid UTF-8: %q", o[len(o)-40:])
		}
		// the characters are valid, so no part of one is shown as bytes
		if strings.Contains(o, `\x`) {
			t.Errorf("a character was cut in two and its bytes shown as escapes: %q", o[len(o)-60:])
		}
	}
}

func FuzzUntrustedOneToOne(f *testing.F) {
	for _, s := range []string{"", "a\nb", `a\nb`, "\xff\xfe", "\u202e", `\...(cut: 1 of 2 bytes shown)`, strings.Repeat("\\", 3000)} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, in string) {
		out := untrusted(in)
		got, cut, shown, err := decodeUntrusted(out)
		if err != nil {
			t.Fatalf("%q printed as %q, which does not read back: %v", in, out, err)
		}
		if len(in) <= maxUntrustedText {
			if cut != 0 || string(got) != in {
				t.Fatalf("%q printed as %q, which reads back as %q (cut %d)", in, out, got, cut)
			}
		} else if cut != len(in) || shown != len(got) || !strings.HasPrefix(in, string(got)) {
			t.Fatalf("a cut value of %d bytes reads back as total %d, shown %d", len(in), cut, shown)
		}
	})
}

// L9-10 and L9-11 (T5, T7, T13, T14): every string that an output function
// shows passes the escaping, in every field, in text and in JSON.
func TestEveryFieldOfTheOutputLayerIsEscaped(t *testing.T) {
	hostile := "x\u009b[2Jy\u202eevil\x7f\u2028VERIFIED: forged\n\xff"
	r := func() *result {
		return &result{cmd: "proof", headline: hostile, steps: []stepJSON{{Name: hostile, OK: true, Detail: hostile}},
			notes: []string{hostile}, notProved: []string{hostile}, problems: []string{hostile}}
	}
	finishers := map[string]func(e *env, asJSON bool){
		"ok":         func(e *env, j bool) { e.finishOK(r(), j) },
		"failed":     func(e *env, j bool) { e.finishFailed(r(), j, hostile, fmt.Errorf("%s", hostile)) },
		"problems":   func(e *env, j bool) { e.finishProblems(r(), j) },
		"incomplete": func(e *env, j bool) { e.finishIncomplete(r(), j, []string{hostile}) },
		"input":      func(e *env, j bool) { e.inputError(hostile, j, fmt.Errorf("%s", hostile)) },
		"input with": func(e *env, j bool) { e.finishInputWith(r(), j, fmt.Errorf("%s", hostile)) },
	}
	for name, f := range finishers {
		for _, asJSON := range []bool{false, true} {
			var out, errb bytes.Buffer
			f(&env{stdout: &out, stderr: &errb}, asJSON)
			for stream, text := range map[string]string{"stdout": out.String(), "stderr": errb.String()} {
				for _, c := range text {
					bad := c == '\x7f' || c == 0x1b || c == '\r' || (c >= 0x80 && c <= 0x9f) || c == 0x202e || c == 0x2028 || c == 0xfeff
					if bad {
						t.Errorf("%s json=%v %s: U+%04X in the output", name, asJSON, stream, c)
					}
				}
				verdicts := 0
				for _, l := range strings.Split(text, "\n") {
					if strings.HasPrefix(l, "VERIFIED:") {
						verdicts++
					}
				}
				// only the real verdict line of a success starts with the word
				if verdicts > 1 || (verdicts == 1 && name != "ok") {
					t.Errorf("%s json=%v %s: %d lines start with the verdict word", name, asJSON, stream, verdicts)
				}
			}
			if asJSON && out.Len() > 0 {
				var o map[string]any
				if err := json.Unmarshal(out.Bytes(), &o); err != nil {
					t.Fatalf("%s: not JSON: %v\n%s", name, err, out.String())
				}
				if o["invalid_utf8"] != true {
					t.Errorf("%s: the invalid byte was replaced without the flag", name)
				}
			}
		}
	}
}

// L9-10 (2): the JSON carries every escaped character as a \u escape, the
// strings read back as written, and the bytes that are not UTF-8 are flagged.
func TestJSONEscapesWhatTheTextEscapes(t *testing.T) {
	for _, tc := range []struct {
		name, in string
		escaped  []string
	}{
		{"C1 control", "a\u009bb", []string{`\u009b`}},
		{"DEL", "a\x7fb", []string{`\u007f`}},
		{"bidirectional", "a\u202eb\u2066c", []string{`\u202e`, `\u2066`}},
		{"separators", "a\u2028b\u2029c", []string{`\u2028`, `\u2029`}},
		{"byte order mark", "\ufeffx", []string{`\ufeff`}},
		{"tag character", "a\U000E0001b", []string{`\udb40\udc01`}},
		{"html characters stay", "<a href=\"x\">&</a>", nil},
		{"plain", "caf\u00e9 \u4e2d", nil},
	} {
		var out bytes.Buffer
		(&env{stdout: &out}).printJSON(jsonOutput{Command: "proof", Error: tc.in, InputErr: true})
		text := out.String()
		for _, c := range text {
			if c == 0x7f || (c >= 0x80 && c <= 0x9f) || c == 0x202e || c == 0x2066 || c == 0x2028 || c == 0x2029 || c == 0xfeff || c == 0xE0001 {
				t.Errorf("%s: U+%04X raw in %q", tc.name, c, text)
			}
		}
		for _, e := range tc.escaped {
			if !strings.Contains(text, e) {
				t.Errorf("%s: %s is not in %q", tc.name, e, text)
			}
		}
		var back jsonOutput
		if err := json.Unmarshal(out.Bytes(), &back); err != nil || back.Error != tc.in {
			t.Errorf("%s: reads back as %q (%v)", tc.name, back.Error, err)
		}
		if back.InvalidUTF8 {
			t.Errorf("%s: flagged as invalid UTF-8", tc.name)
		}
		if strings.Contains(tc.name, "html") && (strings.Contains(text, `\u003c`) || strings.Contains(text, `\u0026`)) {
			t.Errorf("%s: html escapes in %q", tc.name, text)
		}
	}
	// A real U+FFFD in the input is not a replacement: no flag.
	var out bytes.Buffer
	(&env{stdout: &out}).printJSON(jsonOutput{Command: "proof", Error: "a\ufffdb"})
	if strings.Contains(out.String(), "invalid_utf8") {
		t.Error("a real U+FFFD was flagged")
	}
	// A backslash followed by ufffd in the input is text, not a replacement.
	out.Reset()
	(&env{stdout: &out}).printJSON(jsonOutput{Command: "proof", Error: `a\ufffdb`})
	if strings.Contains(out.String(), "invalid_utf8") {
		t.Error("the text \\ufffd was flagged")
	}
	// Bytes that are not UTF-8: U+FFFD and the flag, once.
	out.Reset()
	(&env{stdout: &out}).printJSON(jsonOutput{Command: "proof", Error: "a\xffb", Notes: []string{"x\xfey"}})
	var back jsonOutput
	if err := json.Unmarshal(out.Bytes(), &back); err != nil || !back.InvalidUTF8 || back.Error != "a\ufffdb" {
		t.Errorf("invalid bytes: %+v %v\n%s", back, err, out.String())
	}
	if strings.Count(out.String(), "invalid_utf8") != 1 {
		t.Errorf("the flag is not said once:\n%s", out.String())
	}
}
