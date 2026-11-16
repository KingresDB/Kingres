// SPDX-License-Identifier: AGPL-3.0-only

package cli

import (
	"encoding/json"
	"fmt"
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
}

type stepJSON struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail,omitempty"`
}

func (e *env) printJSON(v any) {
	enc := json.NewEncoder(e.stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
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
	if asJSON {
		e.printJSON(jsonOutput{Command: r.cmd, Verified: true, Steps: r.steps, Info: r.info, Notes: r.notes, NotProved: r.notProved})
		return ExitOK
	}
	word := r.word
	if word == "" {
		word = "VERIFIED"
	}
	fmt.Fprintf(e.stdout, "%s: %s\n", word, untrusted(r.headline))
	e.printSteps(r.steps)
	e.printLines("note: ", r.notes)
	e.printLines("not proved: ", r.notProved)
	return ExitOK
}

// finishFailed prints a failed verification and returns the exit status 1.
func (e *env) finishFailed(r *result, asJSON bool, step string, err error) int {
	steps := append(append([]stepJSON{}, r.steps...), stepJSON{Name: step, OK: false, Detail: err.Error()})
	if asJSON {
		e.printJSON(jsonOutput{Command: r.cmd, Steps: steps, FailedAt: step, Error: err.Error(), Info: r.info, Notes: r.notes})
		return ExitFailed
	}
	fmt.Fprintf(e.stdout, "FAILED at step %s: %s\n", untrusted(step), untrusted(err.Error()))
	e.printSteps(r.steps)
	e.printLines("note: ", r.notes)
	return ExitFailed
}

// finishProblems prints the findings of digest-chain: INCONSISTENT and the
// list, exit status 1.
func (e *env) finishProblems(r *result, asJSON bool) int {
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
	if asJSON {
		e.printJSON(jsonOutput{Command: cmd, InputErr: true, Error: err.Error()})
	}
	e.errLine(cmd, err)
	return ExitUsage
}

// finishInputWith prints what was found and then the input error: the exit
// status is 2 but the reader still sees the steps and notes that explain it.
func (e *env) finishInputWith(r *result, asJSON bool, err error) int {
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
