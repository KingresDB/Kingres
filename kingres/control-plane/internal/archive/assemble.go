// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package archive

import "io"

// Part is one entry of an archive that Assemble writes.
type Part struct {
	Name string
	Data []byte
}

// Assemble writes an archive in the exact shape of the Writer (the fixed entries in their order, then the data entries, then the manifest) with the manifest
// bytes it is given, WITHOUT checking them. It exists so that the tests of the importer can make archives whose manifest lies (a wrong row count, a size that is
// not the file's, an extension outside the closed list): Writer.Finish checks the manifest as the validator does and would refuse to write them. Nothing in the
// server calls it.
func Assemble(dst io.Writer, parts []Part, manifest []byte) error {
	w := NewWriter(dst)
	for _, p := range parts {
		ew, err := w.Create(p.Name)
		if err != nil {
			return err
		}
		if _, err := ew.Write(p.Data); err != nil {
			return err
		}
	}
	if err := w.closeCurrent(); err != nil {
		return err
	}
	e, err := w.start(ManifestName)
	if err != nil {
		return err
	}
	if _, err := e.Write(manifest); err != nil {
		return err
	}
	if err := e.Close(); err != nil {
		return err
	}
	return w.central()
}
