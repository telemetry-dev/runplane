import { describe, expect, test } from 'vitest'
import { hasFilters, jobsApiQuery, pageRange, parseJobsSearch } from './jobs-search'

describe('parseJobsSearch', () => {
  test('keeps known filters', () => {
    expect(
      parseJobsSearch({ q: 'webkit', status: 'failed', repository: 'kestrel-hq/web', pool: 'p1', server: 's1', range: '30d', cursor: 'abc' }),
    ).toEqual({ q: 'webkit', status: 'failed', repository: 'kestrel-hq/web', pool: 'p1', server: 's1', range: '30d', cursor: 'abc' })
  })

  test('drops unknown values, blank text, and the default range, which stays out of the URL', () => {
    // toEqual ignores keys whose value is undefined.
    expect(parseJobsSearch({ status: 'passed', range: '7d', q: '  ', repository: '', sort: 'name' })).toEqual({})
  })

  test('reads a number the router parsed from the URL as text', () => {
    // ?q=4182 arrives as the number 4182.
    expect(parseJobsSearch({ q: 4182 }).q).toBe('4182')
  })
})

describe('jobsApiQuery', () => {
  // Without a range, the API sends the jobs of all time.
  test('asks for the last 7 days when the URL has no range', () => {
    expect(jobsApiQuery({}).range).toBe('7d')
  })

  test('sends no range for all time, and the pool and server as IDs', () => {
    expect(jobsApiQuery({ range: 'all', pool: 'p1', server: 's1', cursor: 'c2' })).toMatchObject({
      range: undefined,
      pool_id: 'p1',
      server_id: 's1',
      cursor: 'c2',
    })
  })
})

describe('hasFilters', () => {
  test.each([
    [{}, false],
    [{ range: '30d' as const, cursor: 'c' }, false],
    [{ q: 'lint' }, true],
    [{ server: 's1' }, true],
  ])('%o: %s', (search, expected) => {
    expect(hasFilters(search)).toBe(expected)
  })
})

test.each([
  [12, 0, 12, '12 jobs'],
  [1, 0, 1, '1 job'],
  [50, 0, 1284, '1–50 of 1,284'],
  [50, 50, 1284, '51–100 of 1,284'],
  [34, 1250, 1284, '1,251–1,284 of 1,284'],
  // A link straight to a later page does not know the pages before it.
  [50, null, 1284, '1,284 jobs'],
] as const)('pageRange(%i, %s, %i) is %j', (count, offset, total, want) => {
  expect(pageRange(count, offset, total)).toBe(want)
})
