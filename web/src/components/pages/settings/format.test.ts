import { expect, test } from 'vitest'
import { relativeTime, shortDateTime } from './format'

const now = new Date('2026-11-04T12:00:00Z').getTime()
const minute = 60_000
const hour = 60 * minute
const day = 24 * hour

test('relative times pick the unit by distance, past and future', () => {
  expect(relativeTime(new Date(now - 20_000), now)).toBe('just now')
  expect(relativeTime(new Date(now - 25 * minute), now)).toBe('25 min ago')
  expect(relativeTime(new Date(now - hour), now)).toBe('1 hour ago')
  expect(relativeTime(new Date(now - 3 * hour), now)).toBe('3 hours ago')
  expect(relativeTime(new Date(now + 4 * day), now)).toBe('in 4 days')
})

test('an invite made a moment ago expires in 7 days, not 6', () => {
  expect(relativeTime(new Date(now + 7 * day - 2_000), now)).toBe('in 7 days')
})

test('after 30 days the date replaces the distance', () => {
  expect(relativeTime(new Date('2026-10-02T12:00:00Z'), now)).toBe('Oct 2, 2026')
})

test('backup times use the 24-hour clock', () => {
  expect(shortDateTime(new Date(2026, 10, 4, 3, 0))).toBe('Nov 4, 03:00')
  expect(shortDateTime(new Date(2026, 10, 2, 16, 41))).toBe('Nov 2, 16:41')
})
