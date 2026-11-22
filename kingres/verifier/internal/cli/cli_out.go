// SPDX-License-Identifier: AGPL-3.0-only

package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"runtime"
	"unicode/utf8"
)

// Every command builds one result and hands it to one of the finish
// functions below, which print it as text or as JSON from the same fields. A
// line that the text output shows (a step, a note, a "not proved" item, a
// missing item, a problem) is therefore in the JSON output too, and the test
// TestTextAndJSONCarryTheSameLines fails when a command prints a line in only
// one of the two formats.
//
// Every string that reaches the text output passes untrusted (text.go): this
// file is the one place where text is written for a person, so that no input
// (a key of a file, a file name, an error text that quotes them) can forge a
// line or move a terminal. TestNoPrintOutsideTheOutputLayer keeps it so.

// result is what a command found, in the form both outputs are made from.
type result struct {
	cmd       string
	word      string // the verdict word of a success, default VERIFIED
	headline  string // after the verdict word
	steps     []stepJSON
	info      map[string]any
	notes     []string // things the reader must know about how this run was made
	notProved []string // what a success does not show
	problems  []string // digest-chain: the findings of a failure
	// keepInfo is set once the checkpoint the auditor holds was judged: the exits
	// that carry no info of their own then carry it (inputInfo), so that
	// old_anchor_weak is on every one of them (L16-02).
	keepInfo bool
	// exit is the status of a success when it is not ExitOK (a verdict with a word of its own).
	exit int
}

// inputInfo is the info an input error carries: the info of the run once the held
// checkpoint was judged, nothing before.
func (r *result) inputInfo() map[string]any {
	if r.keepInfo {
		return r.info
	}
	return nil
}

func (r *result) ok(name, detail string) {
	r.steps = append(r.steps, stepJSON{Name: name, OK: true, Detail: detail})
}

// jsonOutput is the JSON form of every result.
type jsonOutput struct {
	Command   string     `json:"command"`
	Verified  bool       `json:"verified"`
	Steps     []stepJSON `json:"steps,omitempty"`
	Problems  []string   `json:"problems,omitempty"`
	FailedAt  string     `json:"failed_step,omitempty"`
	Error     string     `json:"error,omitempty"`
	InputErr  bool       `json:"input_error,omitempty"`
	Info      any        `json:"info,omitempty"`
	Notes     []string   `json:"notes,omitempty"`
	NotProved []string   `json:"not_proved,omitempty"`
	// Incomplete and Missing are used by exit status 3 only.
	Incomplete bool     `json:"incomplete,omitempty"`
	Missing    []string `json:"missing,omitempty"`
	// InvalidUTF8 is set when a string of the output held bytes that are not
	// UTF-8 (a file name, a text of the database) and the JSON, which has no way
	// to carry them, shows each as U+FFFD. The text output shows such a byte as
	// \xNN; here the loss is said once, in this member.
	InvalidUTF8 bool `json:"invalid_utf8,omitempty"`
}

type stepJSON struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail,omitempty"`
}

// printJSON writes the result as JSON. The JSON is escaped as far as the text
// output is: every character that untrusted writes as an escape is written as a
// \u escape here (a C1 control, DEL, a bidirectional mark, a separator, a format
// character), so that a file that shows the JSON on a terminal cannot be moved
// by it; the characters <, > and & are not rewritten (the output is not HTML).
// Bytes that are not UTF-8 cannot be in a JSON string: they become U+FFFD and
// the member invalid_utf8 says so.
func (e *env) printJSON(v jsonOutput) {
	v.InvalidUTF8 = holdsInvalidUTF8(reflect.ValueOf(v))
	b := encodeJSON(v)
	_, _ = e.stdout.Write(escapeJSONText(b))
}

func encodeJSON(v jsonOutput) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
	return buf.Bytes()
}

// holdsInvalidUTF8 reports whether any string of the value, a key of a map
// included, has bytes that are not UTF-8. It looks at the strings before they
// are encoded because the encoder replaces such bytes by U+FFFD without a word,
// and a U+FFFD that was in the input must not be reported as a replacement.
func holdsInvalidUTF8(v reflect.Value) bool {
	switch v.Kind() {
	case reflect.String:
		return !utf8.ValidString(v.String())
	case reflect.Interface, reflect.Pointer:
		return !v.IsNil() && holdsInvalidUTF8(v.Elem())
	case reflect.Slice, reflect.Array:
		for i := 0; i < v.Len(); i++ {
			if holdsInvalidUTF8(v.Index(i)) {
				return true
			}
		}
	case reflect.Map:
		for it := v.MapRange(); it.Next(); {
			if holdsInvalidUTF8(it.Key()) || holdsInvalidUTF8(it.Value()) {
				return true
			}
		}
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			if v.Type().Field(i).IsExported() && holdsInvalidUTF8(v.Field(i)) {
				return true
			}
		}
	}
	return false
}

// escapeJSONText rewrites every character of an encoded document that
// mustEscape names as a \u escape (a pair of them above U+FFFF).
func escapeJSONText(b []byte) []byte {
	var out bytes.Buffer
	out.Grow(len(b))
	for i := 0; i < len(b); {
		r, n := utf8.DecodeRune(b[i:])
		switch {
		case r == utf8.RuneError && n <= 1:
			out.WriteString(`\ufffd`)
		case r < 0x7f || !mustEscape(r):
			// ASCII controls inside strings were escaped by the encoder; the
			// newlines and spaces that are left are the layout of the document.
			out.Write(b[i : i+n])
		case r > 0xffff:
			r -= 0x10000
			fmt.Fprintf(&out, `\u%04x\u%04x`, 0xd800+(r>>10), 0xdc00+(r&0x3ff))
		default:
			fmt.Fprintf(&out, `\u%04x`, r)
		}
		i += n
	}
	return out.Bytes()
}

// errLine is the one line that reports a usage or input problem on the
// standard error.
func (e *env) errLine(cmd string, err error) {
	fmt.Fprintf(e.stderr, "kingres-verify %s: %s\n", untrusted(cmd), untrusted(err.Error()))
}

func (e *env) printSteps(steps []stepJSON) {
	for _, s := range steps {
		if s.OK {
			fmt.Fprintf(e.stdout, "  ok  %-16s %s\n", untrusted(s.Name), untrusted(s.Detail))
		}
	}
}

func (e *env) printLines(prefix string, lines []string) {
	for _, l := range lines {
		fmt.Fprintf(e.stdout, "  %s%s\n", prefix, untrusted(l))
	}
}

// finishOK prints a verified result and returns the exit status 0.
func (e *env) finishOK(r *result, asJSON bool) int {
	code := ExitOK
	if r.exit != 0 {
		code = r.exit
	}
	traceExit(code)
	if asJSON {
		e.printJSON(jsonOutput{Command: r.cmd, Verified: true, Steps: r.steps, Info: r.info, Notes: r.notes, NotProved: r.notProved})
		return code
	}
	word := r.word
	if word == "" {
		word = "VERIFIED"
	}
	fmt.Fprintf(e.stdout, "%s: %s\n", word, untrusted(r.headline))
	e.printSteps(r.steps)
	e.printLines("note: ", r.notes)
	e.printLines("not proved: ", r.notProved)
	return code
}

// finishFailed prints a failed verification and returns the exit status 1.
func (e *env) finishFailed(r *result, asJSON bool, step string, err error) int {
	return e.finishFailedWith(r, asJSON, step, err, ExitFailed)
}

// finishFailedWith is finishFailed with the status of the caller: the same
// output, a status that says more than 1.
func (e *env) finishFailedWith(r *result, asJSON bool, step string, err error, code int) int {
	traceExit(code)
	steps := append(append([]stepJSON{}, r.steps...), stepJSON{Name: step, OK: false, Detail: err.Error()})
	if asJSON {
		e.printJSON(jsonOutput{Command: r.cmd, Steps: steps, FailedAt: step, Error: err.Error(), Info: r.info, Notes: r.notes})
		return code
	}
	fmt.Fprintf(e.stdout, "FAILED at step %s: %s\n", untrusted(step), untrusted(err.Error()))
	e.printSteps(r.steps)
	e.printLines("note: ", r.notes)
	return code
}

// finishProblems prints the findings of digest-chain: INCONSISTENT and the
// list, exit status 1.
func (e *env) finishProblems(r *result, asJSON bool) int {
	traceExit(ExitFailed)
	if asJSON {
		e.printJSON(jsonOutput{Command: r.cmd, Problems: r.problems, Info: r.info, Notes: r.notes})
		return ExitFailed
	}
	fmt.Fprintln(e.stdout, "INCONSISTENT:")
	e.printLines("- ", r.problems)
	e.printLines("note: ", r.notes)
	return ExitFailed
}

// finishIncomplete prints a result in which nothing failed but something
// asked for could not be checked, and returns the exit status 3. It is not a
// reassurance: the publisher may have withheld exactly the part that would
// have failed (see the README, "Exit status 3").
func (e *env) finishIncomplete(r *result, asJSON bool, missing []string) int {
	traceExit(ExitIncomplete)
	if asJSON {
		e.printJSON(jsonOutput{Command: r.cmd, Steps: r.steps, Incomplete: true, Missing: missing, Info: r.info, Notes: r.notes, NotProved: r.notProved})
		return ExitIncomplete
	}
	fmt.Fprintln(e.stdout, "INCOMPLETE: a check that was asked for could not be made. Treat this as an alert, not as a pass:")
	fmt.Fprintln(e.stdout, "  a publisher must serve what its signed checkpoint needs, and one that withholds it may be hiding a failure.")
	e.printSteps(r.steps)
	e.printLines("missing: ", missing)
	e.printLines("note: ", r.notes)
	e.printLines("not proved: ", r.notProved)
	return ExitIncomplete
}

// inputError reports a usage or input problem (exit status 2).
func (e *env) inputError(cmd string, asJSON bool, err error) int {
	return e.inputErrorInfo(cmd, asJSON, err, nil)
}

// inputErrorInfo is inputError for a run that already judged the checkpoint the
// auditor holds: the JSON carries info (the member anchor_weak or
// old_anchor_weak, L16-02) so that a machine reader gets it on this exit as on
// every other. The text is the same as inputError's.
func (e *env) inputErrorInfo(cmd string, asJSON bool, err error, info map[string]any) int {
	traceExit(ExitUsage)
	if asJSON {
		out := jsonOutput{Command: cmd, InputErr: true, Error: err.Error()}
		if info != nil {
			out.Info = info
		}
		e.printJSON(out)
	}
	e.errLine(cmd, err)
	return ExitUsage
}

// finishInputWith prints what was found and then the input error: the exit
// status is 2 but the reader still sees the steps and notes that explain it.
func (e *env) finishInputWith(r *result, asJSON bool, err error) int {
	traceExit(ExitUsage)
	if asJSON {
		e.printJSON(jsonOutput{Command: r.cmd, InputErr: true, Error: err.Error(), Steps: r.steps, Info: r.info, Notes: r.notes})
		e.errLine(r.cmd, err)
		return ExitUsage
	}
	e.printSteps(r.steps)
	e.printLines("note: ", r.notes)
	e.errLine(r.cmd, err)
	return ExitUsage
}

// trace is set by a test only. It is called with the exit status and the stack
// of every place that ends a run, so that a test can tell which of the places in
// the source a scenario reached (TestEveryExitSiteHasARowAndAScenario).
var trace func(status int, pcs []uintptr)

// traceExit reports the status a run ends with to the test hook, from the
// function that ends it and every function above it.
func traceExit(status int) {
	if trace == nil {
		return
	}
	pcs := make([]uintptr, 32)
	n := runtime.Callers(2, pcs)
	trace(status, pcs[:n])
}

// exit is the way a run that does not go through a finish function ends with a
// status: usage text, version, a flag error, an unknown command.
func exit(status int) int {
	traceExit(status)
	return status
}
