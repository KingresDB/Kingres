// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package transfer

import (
	"fmt"
	"strings"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/sqlx"
)

// ArchiveCopyOptions are the options of every data entry of a database archive (design "The archive"): UTF-8 CSV with a header, every field quoted and a null
// that is the unquoted marker \N, so that an empty string, a null and a text that is \N are three different things. No value is changed.
const ArchiveCopyOptions = `FORMAT csv, HEADER true, NULL '\N', FORCE_QUOTE *, ENCODING 'UTF8'`

// ArchiveCopySQL is the statement that writes one data entry: COPY of a SELECT of the columns of a table, built from names that passed sqlx. only is true for an
// inheritance parent (FROM ONLY); a partitioned table is read with all its leaves.
func ArchiveCopySQL(schema, table string, columns []string, only bool) (string, error) {
	rel, err := sqlx.QualifiedIdent(schema, table)
	if err != nil {
		return "", err
	}
	if len(columns) == 0 {
		return "", fmt.Errorf("an archive entry needs at least one column")
	}
	sel := make([]string, 0, len(columns))
	seen := map[string]bool{}
	for _, c := range columns {
		if seen[c] {
			return "", fmt.Errorf("column %q is listed twice", c)
		}
		seen[c] = true
		id, err := sqlx.NewIdent(c)
		if err != nil {
			return "", err
		}
		sel = append(sel, id.String())
	}
	from := rel.String()
	if only {
		from = "ONLY " + from
	}
	return "COPY (SELECT " + strings.Join(sel, ", ") + " FROM " + from + ") TO STDOUT WITH (" + ArchiveCopyOptions + ")", nil
}

// ArchiveCopyFromOptions are the options of the load of one data entry of a database archive: the options that wrote it, read back.
const ArchiveCopyFromOptions = `FORMAT csv, HEADER true, NULL '\N', ENCODING 'UTF8'`

// ArchiveCopyFromSQL is the statement that loads one data entry: COPY into the table with the columns of the entry. The names passed sqlx; the options are fixed.
func ArchiveCopyFromSQL(schema, table string, columns []string) (string, error) {
	rel, err := sqlx.QualifiedIdent(schema, table)
	if err != nil {
		return "", err
	}
	if len(columns) == 0 {
		return "", fmt.Errorf("an archive entry needs at least one column")
	}
	cols, err := identList(columns)
	if err != nil || cols == "" {
		return "", fmt.Errorf("the columns are not valid")
	}
	return "COPY " + rel.String() + " (" + cols + ") FROM STDIN WITH (" + ArchiveCopyFromOptions + ")", nil
}
