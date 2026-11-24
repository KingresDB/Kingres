// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co
//
// The guess of the encoding, the delimiter and the header from the first bytes of a file, the preview parser, the memory of
// an unfinished upload in the tab, and the checks of the options of a CSV (console increment CN4).
import { afterEach, describe, expect, it, vi } from 'vitest'
import {
  decodeAs,
  guessCsv,
  guessDelimiter,
  guessHeader,
  parsePreview,
  previewRows,
} from '@/lib/csv-guess'
import {
  findStoredUpload,
  forgetUpload,
  listStoredUploads,
  rememberUpload,
} from '@/lib/upload-store'
import {
  defaultExportForm,
  defaultImportForm,
  exportRequest,
  importRequest,
  nullStringError,
  quoteError,
} from '@/lib/transfer-form'

const bytes = (text: string): Uint8Array => new TextEncoder().encode(text)

describe('parsePreview', () => {
  it('reads quotes, doubled quotes, line breaks inside quotes and both kinds of line end', () => {
    expect(parsePreview('a,"b,c","d ""e"""\r\n1,"x\ny",3\n', ',')).toEqual([
      ['a', 'b,c', 'd "e"'],
      ['1', 'x\ny', '3'],
    ])
  })
  it('stops after the rows asked for and keeps a last record without a line end', () => {
    expect(parsePreview('1,2\n3,4\n5,6', ',', '"', 2)).toEqual([
      ['1', '2'],
      ['3', '4'],
    ])
    expect(parsePreview('1,2\n3,4', ',')).toEqual([
      ['1', '2'],
      ['3', '4'],
    ])
  })
})

describe('guessDelimiter', () => {
  it('finds the delimiter that splits every line the same way', () => {
    expect(guessDelimiter('a;b;c\n1;2;3\n')).toBe(';')
    expect(guessDelimiter('a\tb\n1\t2\n')).toBe('\t')
    expect(guessDelimiter('a|b|c\n1|2|3\n')).toBe('|')
    expect(guessDelimiter('a,b,c\n1,2,3\n')).toBe(',')
  })
  it('is not fooled by a delimiter inside quotes or by decimal commas', () => {
    expect(guessDelimiter('name;price\n"a,b";1,5\n"c,d";2,5\n')).toBe(';')
  })
  it('falls back to a comma for a file with one column', () => {
    expect(guessDelimiter('one\ntwo\n')).toBe(',')
  })
})

describe('guessHeader', () => {
  it('reads names above numbers as a header and numbers in the first record as data', () => {
    expect(
      guessHeader([
        ['id', 'total'],
        ['1', '2.5'],
      ]),
    ).toBe(true)
    expect(
      guessHeader([
        ['1', '2.5'],
        ['2', '3.5'],
      ]),
    ).toBe(false)
    expect(
      guessHeader([
        ['id', 'id'],
        ['1', '2'],
      ]),
    ).toBe(false)
    expect(guessHeader([['id']])).toBe(false)
  })
  it('reads a file of text only as having a header when the first record looks like names and differs from the next', () => {
    expect(
      guessHeader([
        ['name', 'city'],
        ['Ada', 'Paris'],
      ]),
    ).toBe(true)
    expect(
      guessHeader([
        ['Ada', 'Paris'],
        ['Ada', 'Rome'],
      ]),
    ).toBe(false)
  })
})

describe('guessCsv', () => {
  it('guesses UTF-8, the delimiter and the header of a file', () => {
    const text = bytes('id;name\n1;Ada\n2;Bo\n')
    expect(guessCsv(text, text.length)).toMatchObject({
      encoding: 'UTF8',
      delimiter: ';',
      header: true,
      compressed: false,
      bom: false,
      partial: false,
    })
  })
  it('sees a byte order mark, a gzip signature and an empty file', () => {
    expect(guessCsv(new Uint8Array([0xef, 0xbb, 0xbf, 0x61, 0x2c, 0x62, 0x0a]), 7).bom).toBe(true)
    expect(guessCsv(new Uint8Array([0x1f, 0x8b, 8, 0]), 100).compressed).toBe(true)
    expect(guessCsv(new Uint8Array(), 0).empty).toBe(true)
  })
  it('guesses a Windows encoding for bytes that are not UTF-8, and decodes them', () => {
    // "caf" and the byte 0xE9 (e acute in Windows-1252): not valid UTF-8.
    const latin = new Uint8Array([0x63, 0x61, 0x66, 0xe9, 0x2c, 0x31, 0x0a])
    const guess = guessCsv(latin, latin.length)
    expect(guess.encoding).toBe('WIN1252')
    expect(decodeAs(latin, guess.encoding)).toBe('café,1\n')
  })
  it('does not take a character cut by the end of the preview for an error', () => {
    const cut = bytes('a,b\n1,é').slice(0, -1)
    expect(guessCsv(cut, 1000).encoding).toBe('UTF8')
  })
  it('drops the record the preview ends in the middle of', () => {
    const rows = previewRows(bytes('a,b\n1,2\n3,'), 'UTF8', ',', '"', true)
    expect(rows).toEqual([
      ['a', 'b'],
      ['1', '2'],
    ])
  })
  it('decodes with the encoding the person chose, and never throws', () => {
    expect(decodeAs(new Uint8Array([0xc3, 0xa9]), 'UTF8')).toBe('é')
    expect(decodeAs(new Uint8Array([0xff, 0xfe]), 'UTF8')).toContain('�')
  })
})

describe('the memory of an upload', () => {
  const target = { database: 'shop', schema: 'public', table: 'notes' }
  const entry = {
    uploadId: '42',
    user: 'admin',
    fileName: 'a.csv',
    sizeBytes: 10,
    lastModified: 5,
    ...target,
  }
  afterEach(() => {
    vi.restoreAllMocks()
    sessionStorage.clear()
  })

  it('remembers, finds by name, size, time and table, and forgets', () => {
    rememberUpload(entry)
    expect(
      findStoredUpload({ name: 'a.csv', size: 10, lastModified: 5 }, target, 'admin')?.uploadId,
    ).toBe('42')
    expect(
      findStoredUpload({ name: 'a.csv', size: 11, lastModified: 5 }, target, 'admin'),
    ).toBeNull()
    expect(
      findStoredUpload({ name: 'a.csv', size: 10, lastModified: 6 }, target, 'admin'),
    ).toBeNull()
    expect(
      findStoredUpload({ name: 'b.csv', size: 10, lastModified: 5 }, target, 'admin'),
    ).toBeNull()
    expect(
      findStoredUpload(
        { name: 'a.csv', size: 10, lastModified: 5 },
        { ...target, table: 'x' },
        'admin',
      ),
    ).toBeNull()
    forgetUpload('42')
    expect(listStoredUploads('admin')).toEqual([])
  })

  it('works without storage: nothing is remembered and nothing throws', () => {
    vi.spyOn(Storage.prototype, 'getItem').mockImplementation(() => {
      throw new Error('blocked')
    })
    vi.spyOn(Storage.prototype, 'setItem').mockImplementation(() => {
      throw new Error('full')
    })
    expect(() => rememberUpload(entry)).not.toThrow()
    expect(listStoredUploads('admin')).toEqual([])
    expect(
      findStoredUpload({ name: 'a.csv', size: 10, lastModified: 5 }, target, 'admin'),
    ).toBeNull()
  })

  it('ignores a stored value that is not a list of uploads, or an id that is not a number', () => {
    sessionStorage.setItem('kingres.uploads', '{"x":1}')
    expect(listStoredUploads('admin')).toEqual([])
    sessionStorage.setItem(
      'kingres.uploads',
      JSON.stringify([{ ...entry, uploadId: '../x' }, entry]),
    )
    expect(listStoredUploads('admin').map((e) => e.uploadId)).toEqual(['42'])
    sessionStorage.setItem('kingres.uploads', 'not json')
    expect(listStoredUploads('admin')).toEqual([])
  })
})

describe('the options of a CSV', () => {
  it('refuses a null text that is too long, has a line break, a quote or the delimiter', () => {
    expect(nullStringError('', ',')).toBeUndefined()
    expect(nullStringError('NULL', ',')).toBeUndefined()
    expect(nullStringError('x'.repeat(33), ',')).toMatch(/32 bytes/)
    // 11 characters of 3 bytes each are 33 bytes.
    expect(nullStringError('€'.repeat(11), ',')).toMatch(/32 bytes/)
    expect(nullStringError('a\nb', ',')).toMatch(/line break/)
    expect(nullStringError('a"b', ',')).toMatch(/quote/)
    expect(nullStringError('a;b', ';')).toMatch(/delimiter/)
  })
  it('wants one byte for the quote, and an ASCII character below @ for a client-only encoding', () => {
    expect(quoteError('"', 'UTF8')).toBeUndefined()
    expect(quoteError('', 'UTF8')).toMatch(/one byte/)
    expect(quoteError('é', 'UTF8')).toMatch(/one byte/)
    expect(quoteError('"', 'SJIS')).toBeUndefined()
    expect(quoteError('a', 'BIG5')).toMatch(/below @/)
  })
  it('makes the request an export or an import sends', () => {
    expect(exportRequest(defaultExportForm(24))).toEqual({
      delimiter: ',',
      nullString: '',
      encoding: 'UTF8',
      utf8Bom: false,
      header: true,
      compression: 'none',
      spreadsheetSafe: true,
      retentionHours: 24,
    })
    expect(
      exportRequest({
        ...defaultExportForm(5),
        encoding: 'WIN1256',
        utf8Bom: true,
        columns: ['a'],
        acknowledgeOtherRolesCode: true,
      }),
    ).toMatchObject({ encoding: 'WIN1256', columns: ['a'], acknowledgeOtherRolesCode: true })
    expect(
      exportRequest({ ...defaultExportForm(5), encoding: 'WIN1256', utf8Bom: true }),
    ).not.toHaveProperty('utf8Bom')
    expect(importRequest('7', defaultImportForm())).toEqual({
      uploadId: '7',
      header: 'skip',
      delimiter: ',',
      nullString: '',
      quote: '"',
      encoding: 'UTF8',
      skipBom: true,
      keepUpload: false,
    })
    expect(
      importRequest('7', {
        ...defaultImportForm(),
        compression: 'gzip',
        forceNull: ['a'],
        columns: ['b', 'a'],
      }),
    ).toMatchObject({ compression: 'gzip', forceNull: ['a'], columns: ['b', 'a'] })
  })
})

describe('the byte order mark option', () => {
  it('is sent only with UTF8: the server answers 400 for it with another encoding', () => {
    const latin = { ...defaultImportForm(), encoding: 'LATIN1' as const, skipBom: true }
    expect(importRequest('7', latin).skipBom).toBe(false)
    expect(importRequest('7', { ...defaultImportForm(), skipBom: true }).skipBom).toBe(true)
    expect(importRequest('7', { ...defaultImportForm(), skipBom: false }).skipBom).toBe(false)
  })
})
