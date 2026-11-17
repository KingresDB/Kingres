// SPDX-License-Identifier: AGPL-3.0-only

package checkpoint

import (
	"errors"
	"strings"
	"testing"

	"github.com/KingresDB/Kingres/kingres/verifier/internal/signednote"
)

// VerifyHeld judges a note the auditor holds by the signatures that verify and
// by nothing else (L10-04, L10-05). Every outcome of it is tested here, and the
// limit of extension lines at its exact value.
func TestVerifyHeld(t *testing.T) {
	priv1, k1 := keyPair(1)
	_, k2 := keyPair(2)
	text := origin + "\n5\n" + rootB64 + "\n"
	good := text + "\n" + sign(origin, priv1, text)

	cp, signers, wrong, parsed, err := VerifyHeld([]byte(good), []signednote.Key{k1, k2}, origin)
	if err != nil || cp.Size != 5 || len(signers) != 1 || wrong != 0 || parsed == nil || len(parsed.Signatures) != 1 {
		t.Fatalf("a good note: %v %v %d %v", cp, signers, wrong, err)
	}
	// a line of a key that is in the set and does not verify is counted, not fatal
	_, k3 := keyPair(3)
	other := sign(origin, priv1, "another text\n") // a signature of key 1 over other text
	twoOfOne := text + "\n" + other                // only line, wrong
	if _, _, w, _, err := VerifyHeld([]byte(twoOfOne), []signednote.Key{k1}, origin); !errors.Is(err, signednote.ErrBadSignature) || w != 1 {
		t.Errorf("a wrong line only: %d %v", w, err)
	}
	// not a note at all: step parse, and the count is not reported
	_, _, _, _, err = VerifyHeld([]byte("no note"), []signednote.Key{k1}, origin)
	var ce *Error
	if !errors.As(err, &ce) || ce.Step != StepParse {
		t.Errorf("a malformed note: %v", err)
	}
	// no key of the set signed it: step signature
	if _, _, _, _, err = VerifyHeld([]byte(good), []signednote.Key{k2, k3}, origin); !errors.As(err, &ce) || ce.Step != StepSignature || !errors.Is(err, signednote.ErrNoTrustedSignature) {
		t.Errorf("no signature of a key of the set: %v", err)
	}
	// a signed text that is not a checkpoint: step checkpoint
	bad := "only one line\n"
	badNote := bad + "\n" + sign(origin, priv1, bad)
	if _, _, _, _, err = VerifyHeld([]byte(badNote), []signednote.Key{k1}, origin); !errors.As(err, &ce) || ce.Step != StepCheckpoint {
		t.Errorf("a signed text that is not a checkpoint: %v", err)
	}
	// another log: step origin
	if _, _, _, _, err = VerifyHeld([]byte(good), []signednote.Key{k1}, "ledger.example.com/kingres/c/0304/e1"); !errors.As(err, &ce) || ce.Step != StepOrigin {
		t.Errorf("another origin: %v", err)
	}
}

func TestExtensionLinesLimitIsExact(t *testing.T) {
	text := func(n int) []byte {
		return []byte(origin + "\n5\n" + rootB64 + "\n" + strings.Repeat("ext\n", n))
	}
	if cp, err := Parse(text(MaxExtensionLines)); err != nil || len(cp.Extensions) != MaxExtensionLines {
		t.Errorf("%d extension lines: %v", MaxExtensionLines, err)
	}
	if _, err := Parse(text(MaxExtensionLines + 1)); err == nil || !strings.Contains(err.Error(), "has 17 extension lines") {
		t.Errorf("%d extension lines: %v", MaxExtensionLines+1, err)
	}
	// the message of an empty line names the line (1-based)
	if _, err := Parse([]byte(origin + "\n5\n" + rootB64 + "\n\next\n")); err == nil || !strings.Contains(err.Error(), "line 4 is empty") {
		t.Errorf("an empty line: %v", err)
	}
}
