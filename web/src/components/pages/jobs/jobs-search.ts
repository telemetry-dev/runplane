import type { JobsQuery } from '~/api/jobs'
import { queryLimits } from '~/api/limits'
import { jobStatusNames } from '~/components/app/status-icon'
import { formatCount } from '~/lib/format'

// Search params of the Jobs page. Each filter lives in the URL, so a filtered
// list can be shared and survives a reload.

export type JobsStatus = NonNullable<JobsQuery['status']>
/** Time range of the list: a range of the API, or all time. */
export type JobsRange = NonNullable<JobsQuery['range']> | 'all'

/** The range of a URL without one. */
export const defaultRange = '7d' satisfies JobsRange

export type JobsSearch = {
  q?: string
  status?: JobsStatus
  repository?: string
  /** Pool ID, shown as the "Runner" filter. */
  pool?: string
  server?: string
  range?: Exclude<JobsRange, typeof defaultRange>
  /** next_cursor of the page before. */
  cursor?: string
}

const statuses: readonly JobsStatus[] = queryLimits['get /api/jobs'].status.enum
const ranges: readonly JobsRange[] = [...queryLimits['get /api/jobs'].range.enum, 'all']

export const statusOptions = statuses.map((value) => ({ value, label: jobStatusNames[value] }))

const rangeNames: Record<JobsRange, string> = {
  '24h': 'Last 24 hours',
  '7d': 'Last 7 days',
  '30d': 'Last 30 days',
  all: 'All time',
}

export const rangeOptions = ranges.map((value) => ({ value, label: rangeNames[value] }))

export const pageSize = 50

/** validateSearch of the route: keeps known values and drops the rest. */
export function parseJobsSearch(search: Record<string, unknown>): JobsSearch {
  return {
    q: text(search.q),
    status: oneOf(search.status, statuses),
    repository: text(search.repository),
    pool: text(search.pool),
    server: text(search.server),
    range: oneOf(search.range, ranges.filter((range) => range !== defaultRange)),
    cursor: text(search.cursor),
  }
}

/** The GET /api/jobs query for the search params. */
export function jobsApiQuery(search: JobsSearch): JobsQuery {
  const range = search.range ?? defaultRange
  return {
    q: search.q,
    status: search.status,
    repository: search.repository,
    pool_id: search.pool,
    server_id: search.server,
    range: range === 'all' ? undefined : range,
    cursor: search.cursor,
    limit: pageSize,
  }
}

/** True when the search or a filter narrows the list. The time range does not count. */
export function hasFilters(search: JobsSearch): boolean {
  return Boolean(search.q || search.status || search.repository || search.pool || search.server)
}

function text(value: unknown): string | undefined {
  // The router parses a value such as "412" as a number.
  if (typeof value === 'number') return String(value)
  return typeof value === 'string' && value.trim() !== '' ? value : undefined
}

function oneOf<T extends string>(value: unknown, allowed: readonly T[]): T | undefined {
  return allowed.find((item) => item === value)
}

/** "1–50 of 1,284", or "12 jobs" when one page holds them all. Without the jobs before this page, only "of 1,284". */
export function pageRange(count: number, offset: number | null, total: number): string {
  if (count === total && !offset) return `${formatCount(total)} ${total === 1 ? 'job' : 'jobs'}`
  if (offset === null) return `${formatCount(total)} jobs`
  return `${formatCount(offset + 1)}–${formatCount(offset + count)} of ${formatCount(total)}`
}
