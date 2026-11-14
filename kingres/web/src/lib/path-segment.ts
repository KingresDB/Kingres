// SPDX-License-Identifier: PostgreSQL
// Copyright (c) 2026, RooyeKhat Media Co
//
// Names that come from the database can be anything a quoted identifier can be, including `.`
// and `..`. Browsers remove dot segments from a URL (also when written `%2E`), so a request for
// such an object would go to a different URL. These names cannot be addressed by URL at all.

/** True when the value can be sent as one URL path segment and still address that exact object. */
export function isAddressableSegment(value: string): boolean {
  return value !== '' && value !== '.' && value !== '..'
}
