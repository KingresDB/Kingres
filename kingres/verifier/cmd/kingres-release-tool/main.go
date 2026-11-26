// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

// Command kingres-release-tool is the signing side of a release of
// kingres-verify. It is not part of a release and is not installed: the
// release script (scripts/release.sh) runs it with "go run" on the machine of
// the person who holds the release key.
//
//	kingres-release-tool keygen --out FILE        make a key file (refuses to overwrite)
//	kingres-release-tool pub    --key-file FILE   print the public key of a key file
//	kingres-release-tool sums   --out FILE NAME...  write the sums of the named files
//	kingres-release-tool sign   --key-file FILE --sums FILE --out FILE
//
// The key file holds the base64 of the 32 byte Ed25519 seed and a newline, mode
// 0600; a key file that others can read is refused on Unix. The tool never
// prints a private key and never takes one on the command line.
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/KingresDB/Kingres/kingres/verifier/internal/release"
)

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: kingres-release-tool keygen|pub|sums|sign [flags]")
		return 2
	}
	var err error
	switch args[0] {
	case "keygen":
		err = keygen(args[1:], stdout)
	case "pub":
		err = pub(args[1:], stdout)
	case "sums":
		err = sums(args[1:])
	case "sign":
		err = sign(args[1:])
	default:
		err = fmt.Errorf("unknown command %q", args[0])
	}
	if err != nil {
		fmt.Fprintln(stderr, "kingres-release-tool:", err)
		return 2
	}
	return 0
}

func flags(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	return fs
}

func keygen(args []string, stdout io.Writer) error {
	fs := flags("keygen")
	out := fs.String("out", "", "key file to create")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *out == "" || fs.NArg() != 0 {
		return errors.New("keygen needs --out FILE")
	}
	seed := make([]byte, ed25519.SeedSize)
	if _, err := rand.Read(seed); err != nil {
		return err
	}
	f, err := os.OpenFile(*out, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, werr := f.WriteString(base64.StdEncoding.EncodeToString(seed) + "\n")
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		_ = os.Remove(*out)
		return werr
	}
	return printPub(ed25519.NewKeyFromSeed(seed), stdout)
}

func readKey(path string) (ed25519.PrivateKey, error) {
	if path == "" {
		return nil, errors.New("missing --key-file")
	}
	fi, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", path)
	}
	if runtime.GOOS != "windows" && fi.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("%s can be read by others (mode %o): make it 0600", path, fi.Mode().Perm())
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	seed, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(raw)))
	if err != nil || len(seed) != ed25519.SeedSize {
		return nil, fmt.Errorf("%s does not hold the base64 of a 32 byte seed", path)
	}
	return ed25519.NewKeyFromSeed(seed), nil
}

func printPub(priv ed25519.PrivateKey, stdout io.Writer) error {
	p := priv.Public().(ed25519.PublicKey)
	fp := sha256.Sum256(p)
	_, err := fmt.Fprintf(stdout, "key name:    %s\npublic key:  %s\nkey string:  %s\nfingerprint: %x\n",
		release.KeyName, base64.StdEncoding.EncodeToString(p), release.VKey(p), fp)
	return err
}

func pub(args []string, stdout io.Writer) error {
	fs := flags("pub")
	kf := fs.String("key-file", "", "key file")
	if err := fs.Parse(args); err != nil {
		return err
	}
	priv, err := readKey(*kf)
	if err != nil {
		return err
	}
	return printPub(priv, stdout)
}

func sums(args []string) error {
	fs := flags("sums")
	out := fs.String("out", "", "sums file to write")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *out == "" || fs.NArg() == 0 {
		return errors.New("sums needs --out FILE and the files to sum")
	}
	var es []release.Entry
	for _, p := range fs.Args() {
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		es = append(es, release.Entry{Hash: sha256.Sum256(b), Name: filepath.Base(p)})
	}
	text, err := release.FormatSums(es)
	if err != nil {
		return err
	}
	return writeNew(*out, text)
}

func sign(args []string) error {
	fs := flags("sign")
	kf := fs.String("key-file", "", "key file")
	sumsPath := fs.String("sums", "", "the sums file to sign")
	out := fs.String("out", "", "note file to write")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *sumsPath == "" || *out == "" || fs.NArg() != 0 {
		return errors.New("sign needs --key-file, --sums and --out")
	}
	priv, err := readKey(*kf)
	if err != nil {
		return err
	}
	text, err := os.ReadFile(*sumsPath)
	if err != nil {
		return err
	}
	note, err := release.Sign(priv, text)
	if err != nil {
		return err
	}
	return writeNew(*out, note)
}

// writeNew writes a file that must not exist yet: a release is never
// overwritten in place.
func writeNew(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	_, werr := f.Write(data)
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		_ = os.Remove(path)
	}
	return werr
}
