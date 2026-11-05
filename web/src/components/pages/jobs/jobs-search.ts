import type { JobsQuery } from '~/api/jobs'
import { jobStatusNames } from '~/components/app/status-icon'
import { formatCount } from '~/lib/format'

// Search params of the Jobs page. Each filter lives in the URL, so a filtered
// list can be shared and survives a reload.

export type JobsStatus = NonNullable<JobsQuery['status']>
/** Time range of the list. `7d` is the default and stays out of the URL. */
export type JobsRange = '24h' | '7d' | '30d' | 'all'

export type JobsSearch = {
  q?: string
  status?: JobsStatus
  repository?: string
  /** Pool ID, shown as the "Runner" filter. */
  pool?: string
  server?: string
  range?: Exclude<JobsRange, '7d'>
  /** next_cursor of the page before. */
  cursor?: string
}

const statuses: readonly JobsStatus[] = ['running', 'queued', 'succeeded', 'failed', 'cancelled']

export const statusOptions = statuses.map((value) => ({ value, label: jobStatusNames[value] }))

export const rangeOptions: ReadonlyArray<{ value: JobsRange; label: string }> = [
  { value: '24h', label: 'Last 24 hours' },
  { value: '7d', label: 'Last 7 days' },
  { value: '30d', label: 'Last 30 days' },
  { value: 'all', label: 'All time' },
]

export const pageSize = 50

/** validateSearch of the route: keeps known values and drops the rest. */
export function parseJobsSearch(search: Record<string, unknown>): JobsSearch {
  return {
    q: text(search.q),
    status: oneOf(search.status, statuses),
    repository: text(search.repository),
    pool: text(search.pool),
    server: text(search.server),
    range: oneOf(search.range, ['24h', '30d', 'all'] as const),
    cursor: text(search.cursor),
  }
}

/** The GET /api/jobs query for the search params. */
export function jobsApiQuery(search: JobsSearch): JobsQuery {
  const range = search.range ?? '7d'
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
