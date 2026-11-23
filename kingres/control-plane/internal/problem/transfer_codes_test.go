// SPDX-License-Identifier: AGPL-3.0-only

package problem

import "testing"

// The codes of contract 0.19.0 (data transfer) are in the ProblemCode enum with the statuses the contract lists for them.
func TestTransferCodesOfContract019(t *testing.T) {
	for _, tc := range []struct {
		c      Code
		want   string
		status int
	}{
		{DataTransferDisabled, "data_transfer_disabled", 403},
		{InsecureTransport, "insecure_transport", 403},
		{TransferQuotaExceeded, "transfer_quota_exceeded", 409},
		{UploadIncomplete, "upload_incomplete", 409},
		{UploadOffsetMismatch, "upload_offset_mismatch", 409},
		{ChunkTooLarge, "chunk_too_large", 413},
		{UploadChecksumMismatch, "upload_checksum_mismatch", 422},
		{ImportHeaderMismatch, "import_header_mismatch", 422},
		{ImportLedgerPolicy, "import_ledger_policy", 422},
		{ImportTooLarge, "import_too_large", 422},
		{ImportEndMarker, "import_end_marker", 422},
		{ExportUntrustedCode, "export_untrusted_code", 422},
	} {
		p := New(tc.c, "d")
		if string(p.Code) != tc.want || p.Status != tc.status || p.Title == "" || p.Type != "https://kingresdb.dev/problems/"+slug(tc.want) {
			t.Errorf("%s: %+v", tc.want, p)
		}
	}
}
