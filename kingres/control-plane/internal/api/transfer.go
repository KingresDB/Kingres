// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package api

import "time"

// The types of the data transfer (contract 0.19.0). Identifiers are decimal strings.

// ExportTableRequest is the body of exportTable.
type ExportTableRequest struct {
	Columns                   []string `json:"columns,omitempty"`
	Delimiter                 string   `json:"delimiter,omitempty"`
	NullString                *string  `json:"nullString,omitempty"`
	Quote                     string   `json:"quote,omitempty"`
	Header                    *bool    `json:"header,omitempty"`
	Encoding                  string   `json:"encoding,omitempty"`
	UTF8BOM                   bool     `json:"utf8Bom,omitempty"`
	Compression               string   `json:"compression,omitempty"`
	SpreadsheetSafe           *bool    `json:"spreadsheetSafe,omitempty"`
	AcknowledgeOtherRolesCode bool     `json:"acknowledgeOtherRolesCode,omitempty"`
	RetentionHours            *int     `json:"retentionHours,omitempty"`
}

// ImportTableRequest is the body of importTable.
type ImportTableRequest struct {
	UploadID     string   `json:"uploadId"`
	Header       string   `json:"header,omitempty"`
	Columns      []string `json:"columns,omitempty"`
	Delimiter    string   `json:"delimiter,omitempty"`
	NullString   *string  `json:"nullString,omitempty"`
	Quote        string   `json:"quote,omitempty"`
	Encoding     string   `json:"encoding,omitempty"`
	Compression  string   `json:"compression,omitempty"`
	ForceNotNull []string `json:"forceNotNull,omitempty"`
	ForceNull    []string `json:"forceNull,omitempty"`
	SkipBOM      *bool    `json:"skipBom,omitempty"`
	KeepUpload   bool     `json:"keepUpload,omitempty"`
}

// CreateDataUploadRequest is the body of createDataUpload.
type CreateDataUploadRequest struct {
	Purpose   string `json:"purpose"`
	SizeBytes int64  `json:"sizeBytes"`
	SHA256    string `json:"sha256,omitempty"`
	FileName  string `json:"fileName,omitempty"`
}

// DataUpload is a resumable upload.
type DataUpload struct {
	UploadID      string    `json:"uploadId"`
	State         string    `json:"state"`
	SizeBytes     int64     `json:"sizeBytes"`
	Offset        int64     `json:"offset"`
	ChunkBytesMax int64     `json:"chunkBytesMax"`
	ChunkAlign    int       `json:"chunkAlign"`
	SHA256        *string   `json:"sha256"`
	ExpiresAt     time.Time `json:"expiresAt"`
	// Report is the archive report of the last validation (contract 0.29.0); null before one.
	Report *ArchiveReport `json:"report"`
}

// TableConsistency is what a file guarantees for one table.
type TableConsistency struct {
	Table  string `json:"table"`
	Level  string `json:"level"`
	Groups int    `json:"groups"`
}

// DataExport is a staged export file.
type DataExport struct {
	ExportID    string             `json:"exportId"`
	Kind        string             `json:"kind"`
	State       string             `json:"state"`
	Database    *string            `json:"database"`
	Schema      *string            `json:"schema"`
	Table       *string            `json:"table"`
	SizeBytes   *int64             `json:"sizeBytes"`
	SHA256      *string            `json:"sha256"`
	Rows        *int64             `json:"rows"`
	Consistency []TableConsistency `json:"consistency"`
	RLSApplied  bool               `json:"rlsApplied"`
	// SpreadsheetSafe says whether the file was written with the spreadsheet guard (null while it is built).
	SpreadsheetSafe *bool      `json:"spreadsheetSafe"`
	Format          string     `json:"format,omitempty"`
	TaskID          int64      `json:"taskId,omitempty"`
	CreatedAt       time.Time  `json:"createdAt"`
	ExpiresAt       time.Time  `json:"expiresAt"`
	DownloadedAt    *time.Time `json:"downloadedAt"`
	// Warnings are the warnings of the manifest of a database archive (contract 0.29.0).
	Warnings []string `json:"warnings,omitempty"`
}

// DataExportPage is a page of exports.
type DataExportPage struct {
	Items      []DataExport `json:"items"`
	NextCursor *string      `json:"nextCursor"`
}

// The types of the database export and the archive validation (contract 0.29.0).

// ExportDatabaseRequest is the body of exportDatabase.
type ExportDatabaseRequest struct {
	ExcludeTables               []TableRef `json:"excludeTables,omitempty"`
	DataMode                    string     `json:"dataMode,omitempty"`
	AcknowledgeLargeObjectsLost bool       `json:"acknowledgeLargeObjectsLost,omitempty"`
	RetentionHours              *int       `json:"retentionHours,omitempty"`
}

// ArchiveExtension is an extension of an archive and the version installed here.
type ArchiveExtension struct {
	Name      string  `json:"name"`
	Version   string  `json:"version"`
	Schema    string  `json:"schema"`
	Installed *string `json:"installed"`
}

// ArchiveVersions are the versions and the locale of the source of an archive.
type ArchiveVersions struct {
	PostgresMajor  int                `json:"postgresMajor"`
	KingresVersion string             `json:"kingresVersion"`
	Extensions     []ArchiveExtension `json:"extensions"`
	Encoding       string             `json:"encoding"`
	LcCollate      string             `json:"lcCollate"`
	LcCtype        string             `json:"lcCtype"`
}

// ArchiveTables counts the tables of an archive by kind.
type ArchiveTables struct {
	Local     int   `json:"local"`
	Hash      int   `json:"hash"`
	Reference int   `json:"reference"`
	Ledger    int   `json:"ledger"`
	Columnar  int   `json:"columnar"`
	Rows      int64 `json:"rows"`
	Bytes     int64 `json:"bytes"`
}

// RefusedStatement is a statement the shape grammar refused: its kind, where it is and a fingerprint, never its text.
type RefusedStatement struct {
	Kind        string `json:"kind"`
	Position    string `json:"position"`
	Fingerprint string `json:"fingerprint"`
	Reason      string `json:"reason"`
}

// ArchiveStatements is what the shape grammar found in the SQL files of an archive.
type ArchiveStatements struct {
	ByKind  map[string]int     `json:"byKind"`
	Refused []RefusedStatement `json:"refused"`
}

// ArchiveNeeds says what an import of the archive needs from the user.
type ArchiveNeeds struct {
	LedgerRestart    bool `json:"ledgerRestart"`
	LargeObjectsLost bool `json:"largeObjectsLost"`
}

// ArchiveReport is the result of the validation of an archive.
type ArchiveReport struct {
	Valid      bool               `json:"valid"`
	Format     string             `json:"format"`
	SHA256     string             `json:"sha256,omitempty"`
	Versions   ArchiveVersions    `json:"versions"`
	Tables     ArchiveTables      `json:"tables"`
	Statements *ArchiveStatements `json:"statements,omitempty"`
	Needs      *ArchiveNeeds      `json:"needs,omitempty"`
	Warnings   []string           `json:"warnings,omitempty"`
	Reasons    []TableBlocker     `json:"reasons"`
}

// ImportDatabaseRequest is the body of importDatabase (contract 0.30.0). It is a closed schema: there is no privileges, no allowCode and no bucketCount.
type ImportDatabaseRequest struct {
	UploadID                 string `json:"uploadId"`
	Database                 string `json:"database,omitempty"`
	ExpectedSHA256           string `json:"expectedSha256,omitempty"`
	AcknowledgeLedgerRestart bool   `json:"acknowledgeLedgerRestart,omitempty"`
	OnForeignKeyViolation    string `json:"onForeignKeyViolation,omitempty"`
	KeepOnFailure            bool   `json:"keepOnFailure,omitempty"`
}
