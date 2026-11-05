import type { Schemas } from '~/api/client'
import { queryLimits } from '~/api/limits'
import type { BarChartBar } from '~/components/app/bar-chart'
import { formatCount, formatDate, formatDuration, formatPercent } from '~/lib/format'

// Turns the GET /api/overview response into what the page shows.

export type Overview = Schemas['Overview']
export type Range = Overview['range']
type Bucket = Schemas['OverviewBucket']

const rangeLimits = queryLimits['get /api/overview'].range

export const ranges: readonly Range[] = rangeLimits.enum

/** The range of the API without one, which stays out of the URL. */
export const defaultRange: Range = rangeLimits.default

/** validateSearch of the Overview: a range other than the default, or none. */
export function parseOverviewSearch(search: Record<string, unknown>): { range?: Range } {
  const range = ranges.find((item) => item === search.range)
  return { range: range === defaultRange ? undefined : range }
}

/**
 * True when runplane has no job to show at all. `recent` holds the last
 * completed jobs of any time, so it is empty only before the first job ends.
 */
export function hasNoJobs(overview: Overview): boolean {
  return overview.stats.jobs === 0 && overview.running.length === 0 && overview.recent.length === 0
}

/** The figures of the stats row. `null` shows an em dash: the range has no data for it. */
export function statValues({ stats, buckets }: Overview) {
  // The failure rate is failed / (succeeded + failed), the same jobs the buckets count.
  const decided = buckets.reduce((sum, bucket) => sum + bucket.succeeded + bucket.failed, 0)
  return {
    jobs: formatCount(stats.jobs),
    medianQueue: stats.median_queue_ms === null ? null : formatDuration(stats.median_queue_ms, { fraction: true }),
    failureRate: decided > 0 ? formatPercent(stats.failure_rate * 100) : null,
    cacheHitRate: stats.cache_hit_rate === null ? null : formatPercent(stats.cache_hit_rate * 100),
  }
}

/** Percent of its usual run time that a running job has used, or null (unknown) without a usual time or past it. */
export function jobProgress(elapsedMs: number, expectedMs: number): number | null {
  if (expectedMs <= 0 || elapsedMs > expectedMs) return null
  return (elapsedMs / expectedMs) * 100
}

type ChartSpec = {
  title: string
  period: string
  /** Bucket size of the API for the range. */
  hours: number
  tickEvery: number
  tick: (start: Date) => string
  label: (start: Date, end: Date) => string
}

const charts: Record<Range, ChartSpec> = {
  '24h': {
    title: 'Jobs per hour',
    period: 'the last 24 hours',
    hours: 1,
    tickEvery: 6,
    tick: time,
    label: (start, end) => `${time(start)} – ${time(end)}`,
  },
  '7d': {
    title: 'Jobs per 6 hours',
    period: 'the last 7 days',
    hours: 6,
    tickEvery: 4,
    tick: day,
    label: (start, end) => `${day(start)}, ${time(start)} – ${time(end)}`,
  },
  '30d': { title: 'Jobs per day', period: 'the last 30 days', hours: 24, tickEvery: 7, tick: day, label: day },
}

/**
 * The bar chart of a range: one bar of completed jobs per bucket, capped by
 * the failed ones, with labels in local time. Ticks count back from the end,
 * so the last one keeps clear of "Now".
 */
export function jobsChart(range: Range, buckets: readonly Bucket[]) {
  const { title, period, hours, tickEvery, tick, label } = charts[range]
  const bars = buckets.map((bucket): BarChartBar => {
    const start = new Date(bucket.start)
    const end = new Date(start.getTime() + hours * 3_600_000)
    const total = bucket.succeeded + bucket.failed + bucket.cancelled
    return { label: label(start, end), value: total, part: bucket.failed, detail: bucketDetail(total, bucket) }
  })
  const ticks: Array<{ at: number; label: string }> = []
  for (let at = buckets.length - tickEvery; at >= 0; at -= tickEvery) {
    ticks.unshift({ at, label: tick(new Date(buckets[at]!.start)) })
  }
  return { title, period, bars, ticks, empty: bars.every((bar) => bar.value === 0) }
}

function bucketDetail(total: number, { failed, cancelled }: Bucket): string {
  if (total === 0) return 'No jobs'
  const parts = [`${formatCount(total)} ${total === 1 ? 'job' : 'jobs'}`, `${formatCount(failed)} failed`]
  if (cancelled > 0) parts.push(`${formatCount(cancelled)} cancelled`)
  return parts.join(' · ')
}

const clock = new Intl.DateTimeFormat('en-US', { hour: '2-digit', minute: '2-digit', hourCycle: 'h23' })

function time(value: Date): string {
  return clock.format(value)
}

function day(value: Date): string {
  return formatDate(value, { year: false })
}
