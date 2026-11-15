// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co
//
// The time zone names of the mock (`pg_timezone_names` of the answering server): name, current UTC offset and whether
// daylight saving time is in effect (a `*` after the offset). About three hundred names, enough to make the zone select
// worth searching. The offsets are those of mid November.

const ZONES = `
Africa/Abidjan +00:00
Africa/Accra +00:00
Africa/Addis_Ababa +03:00
Africa/Algiers +01:00
Africa/Cairo +02:00
Africa/Casablanca +01:00
Africa/Dakar +00:00
Africa/Dar_es_Salaam +03:00
Africa/Johannesburg +02:00
Africa/Khartoum +02:00
Africa/Kinshasa +01:00
Africa/Lagos +01:00
Africa/Luanda +01:00
Africa/Maputo +02:00
Africa/Nairobi +03:00
Africa/Tripoli +02:00
Africa/Tunis +01:00
Africa/Windhoek +02:00
America/Anchorage -09:00*
America/Argentina/Buenos_Aires -03:00
America/Asuncion -03:00
America/Bogota -05:00
America/Caracas -04:00
America/Chicago -06:00
America/Costa_Rica -06:00
America/Denver -07:00
America/Detroit -05:00
America/Edmonton -07:00
America/El_Salvador -06:00
America/Guatemala -06:00
America/Guayaquil -05:00
America/Halifax -04:00
America/Havana -05:00
America/Jamaica -05:00
America/La_Paz -04:00
America/Lima -05:00
America/Los_Angeles -08:00
America/Manaus -04:00
America/Mexico_City -06:00
America/Montevideo -03:00
America/New_York -05:00
America/Panama -05:00
America/Phoenix -07:00
America/Puerto_Rico -04:00
America/Regina -06:00
America/Santiago -03:00*
America/Sao_Paulo -03:00
America/St_Johns -03:30
America/Tijuana -08:00
America/Toronto -05:00
America/Vancouver -08:00
America/Winnipeg -06:00
Antarctica/McMurdo +13:00*
Antarctica/Troll +00:00
Arctic/Longyearbyen +01:00
Asia/Almaty +05:00
Asia/Amman +03:00
Asia/Baghdad +03:00
Asia/Bahrain +03:00
Asia/Baku +04:00
Asia/Bangkok +07:00
Asia/Beirut +02:00
Asia/Brunei +08:00
Asia/Colombo +05:30
Asia/Damascus +03:00
Asia/Dhaka +06:00
Asia/Dubai +04:00
Asia/Hong_Kong +08:00
Asia/Irkutsk +08:00
Asia/Jakarta +07:00
Asia/Jerusalem +02:00
Asia/Kabul +04:30
Asia/Karachi +05:00
Asia/Kathmandu +05:45
Asia/Kolkata +05:30
Asia/Kuala_Lumpur +08:00
Asia/Kuwait +03:00
Asia/Manila +08:00
Asia/Muscat +04:00
Asia/Novosibirsk +07:00
Asia/Phnom_Penh +07:00
Asia/Pyongyang +09:00
Asia/Qatar +03:00
Asia/Riyadh +03:00
Asia/Seoul +09:00
Asia/Shanghai +08:00
Asia/Singapore +08:00
Asia/Taipei +08:00
Asia/Tashkent +05:00
Asia/Tbilisi +04:00
Asia/Tehran +03:30
Asia/Thimphu +06:00
Asia/Tokyo +09:00
Asia/Ulaanbaatar +08:00
Asia/Vladivostok +10:00
Asia/Yangon +06:30
Asia/Yekaterinburg +05:00
Asia/Yerevan +04:00
Atlantic/Azores -01:00
Atlantic/Bermuda -04:00
Atlantic/Canary +00:00
Atlantic/Cape_Verde -01:00
Atlantic/Reykjavik +00:00
Australia/Adelaide +10:30*
Australia/Brisbane +10:00
Australia/Darwin +09:30
Australia/Hobart +11:00*
Australia/Melbourne +11:00*
Australia/Perth +08:00
Australia/Sydney +11:00*
Europe/Amsterdam +01:00
Europe/Athens +02:00
Europe/Belgrade +01:00
Europe/Berlin +01:00
Europe/Brussels +01:00
Europe/Bucharest +02:00
Europe/Budapest +01:00
Europe/Copenhagen +01:00
Europe/Dublin +00:00
Europe/Helsinki +02:00
Europe/Istanbul +03:00
Europe/Kyiv +02:00
Europe/Lisbon +00:00
Europe/Ljubljana +01:00
Europe/London +00:00
Europe/Madrid +01:00
Europe/Minsk +03:00
Europe/Moscow +03:00
Europe/Oslo +01:00
Europe/Paris +01:00
Europe/Prague +01:00
Europe/Riga +02:00
Europe/Rome +01:00
Europe/Sofia +02:00
Europe/Stockholm +01:00
Europe/Tallinn +02:00
Europe/Vienna +01:00
Europe/Vilnius +02:00
Europe/Warsaw +01:00
Europe/Zagreb +01:00
Europe/Zurich +01:00
Indian/Maldives +05:00
Indian/Mauritius +04:00
Pacific/Auckland +13:00*
Pacific/Fiji +12:00
Pacific/Guam +10:00
Pacific/Honolulu -10:00
Pacific/Noumea +11:00
Pacific/Pago_Pago -11:00
Pacific/Port_Moresby +10:00
Pacific/Tahiti -10:00
Pacific/Tongatapu +13:00
UTC +00:00
GMT +00:00
Zulu +00:00
Universal +00:00
Greenwich +00:00
CET +01:00
EET +02:00
MET +01:00
WET +00:00
EST -05:00
MST -07:00
HST -10:00
EST5EDT -05:00
CST6CDT -06:00
MST7MDT -07:00
PST8PDT -08:00
Iran +03:30
Israel +02:00
Japan +09:00
Singapore +08:00
Turkey +03:00
Cuba -05:00
Egypt +02:00
Eire +00:00
Poland +01:00
Portugal +00:00
`

/** Etc/GMT-14 .. Etc/GMT+12: the sign of the name is the reverse of the offset (POSIX). */
function etcZones(): string {
  const lines: string[] = []
  for (let h = -12; h <= 14; h += 1) {
    if (h === 0) {
      lines.push('Etc/GMT +00:00', 'Etc/UTC +00:00', 'Etc/UCT +00:00', 'Etc/Zulu +00:00')
      continue
    }
    const sign = h > 0 ? '+' : '-'
    const name = `Etc/GMT${h > 0 ? '-' : '+'}${Math.abs(h)}`
    lines.push(`${name} ${sign}${String(Math.abs(h)).padStart(2, '0')}:00`)
  }
  return lines.join('\n')
}

export interface MockZone {
  name: string
  utcOffset: string
  isDaylightSaving: boolean
}

export const TIME_ZONES: MockZone[] = `${ZONES}\n${etcZones()}`
  .split('\n')
  .map((l) => l.trim())
  .filter(Boolean)
  .map((line) => {
    const [name, offset] = line.split(' ') as [string, string]
    return {
      name,
      utcOffset: offset.replace('*', ''),
      isDaylightSaving: offset.endsWith('*'),
    }
  })
  .sort((a, b) => (a.name < b.name ? -1 : a.name > b.name ? 1 : 0))

export const ZONE_NAMES: ReadonlySet<string> = new Set(TIME_ZONES.map((z) => z.name))
