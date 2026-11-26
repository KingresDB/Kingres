// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package pgconninfo

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

func TestInjectionIsNeutral(t *testing.T) {
	evil := "x host=attacker.example port=1 sslmode=disable' user=root \\"
	dsn, err := Format(Pair{"host", "127.0.0.1"}, Pair{"port", "5432"}, Pair{"dbname", evil}, Pair{"user", "kingresd"}, Pair{"password", "p w'd\\"})
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Host != "127.0.0.1" || cfg.Port != 5432 || cfg.Database != evil || cfg.User != "kingresd" || cfg.Password != "p w'd\\" {
		t.Fatalf("parsed %q %d %q %q", cfg.Host, cfg.Port, cfg.Database, cfg.User)
	}
}

func TestRefusals(t *testing.T) {
	if _, err := Format(Pair{"dbname", "a\x00b"}); err == nil {
		t.Fatal("NUL accepted")
	}
	if _, err := Format(Pair{"host", "x"}, Pair{"hostaddr", "1.2.3.4"}); err == nil {
		t.Fatal("unknown keyword accepted")
	}
	if _, err := Format(Pair{"dbname x=y", "v"}); err == nil {
		t.Fatal("keyword with a space accepted")
	}
}

func FuzzFormatRoundTrip(f *testing.F) {
	for _, s := range []string{"", "postgres", "a b", "x host=evil", "'", `\`, `\'`, `a'b\c d=e`, "é\n\t"} {
		f.Add(s, s)
	}
	f.Fuzz(func(t *testing.T, db, user string) {
		dsn, err := Format(Pair{"host", "127.0.0.1"}, Pair{"port", "5432"}, Pair{"dbname", db}, Pair{"user", user})
		if strings.IndexByte(db, 0) >= 0 || strings.IndexByte(user, 0) >= 0 {
			if err == nil {
				t.Fatal("NUL accepted")
			}
			return
		}
		if err != nil {
			t.Fatal(err)
		}
		cfg, err := pgx.ParseConfig(dsn)
		if err != nil {
			// libpq's grammar has no empty-host problem here; an empty value is the only
			// thing pgx may reject.
			if db == "" || user == "" {
				return
			}
			t.Fatalf("parse: %v", err)
		}
		if cfg.Host != "127.0.0.1" || cfg.Port != 5432 {
			t.Fatalf("host or port changed: %q %d", cfg.Host, cfg.Port)
		}
		if db != "" && cfg.Database != db {
			t.Fatalf("database %q became %q", db, cfg.Database)
		}
		if user != "" && cfg.User != user {
			t.Fatalf("user %q became %q", user, cfg.User)
		}
	})
}

// A string literal that looks like a connection string, or a URL of the
// postgres scheme.
var connLiteral = regexp.MustCompile(`(^|\s)(host|hostaddr|port|dbname|user|password|sslmode|sslcert|sslkey|sslrootcert|sslcrl|application_name)=|postgres(ql)?://`)

// allowedLiterals are constants that carry no variable part.
var allowedLiterals = map[string]bool{
	"host=127.0.0.1 port=5432 dbname=postgres": true, // the documented default DSN of the configuration
}

// TestNoConnectionStringsAssembledElsewhere fails when a Go file of the module
// outside this package has a string literal (in test files only one with a format verb) that looks like
// a libpq connection string or a postgres URL, other than the listed constants.
func TestNoConnectionStringsAssembledElsewhere(t *testing.T) {
	root := filepath.Join("..", "..")
	fset := token.NewFileSet()
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == "pgconninfo" || d.Name() == "node_modules" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		src, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		file, perr := parser.ParseFile(fset, path, src, 0)
		if perr != nil {
			return perr
		}
		ast.Inspect(file, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			s, uerr := strconv.Unquote(lit.Value)
			if uerr != nil {
				return true
			}
			isTest := strings.HasSuffix(path, "_test.go")
			if connLiteral.MatchString(s) && !allowedLiterals[s] && (!isTest || strings.Contains(s, "%")) {
				t.Errorf("%s: string literal looks like a connection string: use internal/pgconninfo (%.40q)", fset.Position(lit.Pos()), s)
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
