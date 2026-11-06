import { describe, expect, test } from 'vitest'
import {
  formatBytes,
  formatCount,
  formatDateTime,
  formatDuration,
  formatMegabytes,
  formatPercent,
  formatRelativeTime,
  formatTimeAgo,
  formatVersion,
} from './format'

describe('formatDuration', () => {
  test.each([
    [0, '0s'],
    [48_000, '48s'],
    [59_999, '59s'],
    [60_000, '1m 00s'],
    [102_000, '1m 42s'],
    [185_000, '3m 05s'],
    [3_599_000, '59m 59s'],
    [3_600_000, '1h 00m'],
    [7_500_000, '2h 05m'],
    [93_780_000, '26h 03m'],
  ])('%i ms is %s', (ms, want) => {
    expect(formatDuration(ms)).toBe(want)
  })

  test('shows tenths below 10 seconds when asked', () => {
    expect(formatDuration(2_840, { fraction: true })).toBe('2.8s')
    expect(formatDuration(400, { fraction: true })).toBe('0.4s')
    expect(formatDuration(4_000, { fraction: true })).toBe('4s')
    expect(formatDuration(12_300, { fraction: true })).toBe('12s')
    expect(formatDuration(102_000, { fraction: true })).toBe('1m 42s')
  })

  test('treats negative input from clock skew as zero', () => {
    expect(formatDuration(-1_500)).toBe('0s')
  })
})

describe('formatRelativeTime', () => {
  const now = Date.parse('2026-11-04T12:00:00Z')

  test.each([
    ['2026-11-04T12:00:00Z', 'just now'],
    ['2026-11-04T11:59:57Z', 'just now'],
    ['2026-11-04T11:59:48Z', '12s ago'],
    ['2026-11-04T11:58:00Z', '2m ago'],
    ['2026-11-04T11:39:00Z', '21m ago'],
    ['2026-11-04T09:00:00Z', '3h ago'],
    ['2026-11-02T12:00:00Z', '2d ago'],
    ['2026-10-02T12:00:00Z', 'Oct 2'],
    ['2025-10-02T12:00:00Z', 'Oct 2, 2025'],
    ['2026-11-04T12:25:00Z', 'in 25m'],
  ])('%s is %s', (time, want) => {
    expect(formatRelativeTime(time, now)).toBe(want)
  })

  test('accepts Date objects and unix milliseconds', () => {
    expect(formatRelativeTime(new Date(now - 120_000), now)).toBe('2m ago')
    expect(formatRelativeTime(now - 120_000, now)).toBe('2m ago')
  })

  const minute = 60_000
  const hour = 60 * minute
  const day = 24 * hour
  const long = (time: number | string) => formatRelativeTime(time, now, { long: true })

  test('the long form picks the unit by distance, past and future', () => {
    expect(long(now - 20_000)).toBe('just now')
    expect(long(now - 25 * minute)).toBe('25 min ago')
    expect(long(now - hour)).toBe('1 hour ago')
    expect(long(now - 3 * hour)).toBe('3 hours ago')
    expect(long(now + 4 * day)).toBe('in 4 days')
  })

  test('an invite made a moment ago expires in 7 days, not 6', () => {
    expect(long(now + 7 * day - 2_000)).toBe('in 7 days')
  })

  test('in the long form, the date replaces the distance after 30 days', () => {
    expect(long('2026-10-02T12:00:00Z')).toBe('Oct 2, 2026')
  })
})

test('backup times use the 24-hour clock', () => {
  expect(formatDateTime(new Date(2026, 10, 4, 3, 0))).toBe('Nov 4, 03:00')
  expect(formatDateTime(new Date(2026, 10, 2, 16, 41))).toBe('Nov 2, 16:41')
})

describe('formatTimeAgo', () => {
  const now = Date.parse('2026-11-04T12:00:00Z')

  test('a time after a lagging now reads "just now"', () => {
    expect(formatTimeAgo('2026-11-04T12:00:21Z', now)).toBe('just now')
  })

  test('a past time reads as formatRelativeTime does', () => {
    expect(formatTimeAgo('2026-11-04T11:57:00Z', now)).toBe('3m ago')
  })
})

// The tests run in a German locale, which groups with dots (vitest.config.ts).
test('formatCount groups thousands with commas, also in a locale that groups with dots', () => {
  expect(formatCount(1284)).toBe('1,284')
  expect(formatCount(1_250_000)).toBe('1,250,000')
})

describe('formatBytes', () => {
  test.each([
    [0, '0 B'],
    [512, '512 B'],
    [1536, '1.5 KB'],
    [8 * 1024 ** 3, '8 GB'],
    [11.4 * 1024 ** 3, '11.4 GB'],
    [127.9 * 1024 ** 3, '128 GB'],
    [1.9 * 1024 ** 4, '1.9 TB'],
    [3 * 1024 ** 5, '3 PB'],
  ])('%i bytes is %s', (bytes, want) => {
    expect(formatBytes(bytes)).toBe(want)
  })

  test('rounds up to the next unit instead of showing 1024 of the smaller one', () => {
    expect(formatBytes(1024 ** 3 - 1)).toBe('1 GB')
  })

  test('formats megabyte counts from the API', () => {
    expect(formatMegabytes(32_768)).toBe('32 GB')
    expect(formatMegabytes(512)).toBe('512 MB')
  })
})

describe('formatPercent', () => {
  test.each([
    [0, '0%'],
    [4.1, '4.1%'],
    [4.04, '4%'],
    [9.96, '10%'],
    [52.4, '52%'],
    [92, '92%'],
    [100, '100%'],
  ])('%f is %s', (value, want) => {
    expect(formatPercent(value)).toBe(want)
  })
})

describe('formatVersion', () => {
  test.each([
    ['v1.2.0 (3f2a9c1)', 'v1.2.0'],
    ['v1.2.0', 'v1.2.0'],
    ['1.2.0', 'v1.2.0'],
    ['dev (3f2a9c1)', 'dev'],
  ])('%s is %s', (version, want) => {
    expect(formatVersion(version)).toBe(want)
  })
})
