// SPDX-License-Identifier: AGPL-3.0-only

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
}

// DataExportPage is a page of exports.
type DataExportPage struct {
	Items      []DataExport `json:"items"`
	NextCursor *string      `json:"nextCursor"`
}
