import { describe, expect, test } from 'vitest'
import type { Schemas } from '~/api/client'
import { jobProgress, jobsChart, statValues, type Overview, type Range } from './data'

// The chart labels use the local time. vitest.config.ts sets the time zone to
// Asia/Tokyo, thus the bucket starts have the +09:00 offset.

type Bucket = Schemas['OverviewBucket']

function buckets(first: string, hours: number, count: number, fill: Record<number, Partial<Bucket>> = {}): Bucket[] {
  const start = Date.parse(first)
  return Array.from({ length: count }, (_, index) => ({
    start: new Date(start + index * hours * 3_600_000).toISOString(),
    succeeded: 0,
    failed: 0,
    cancelled: 0,
    ...fill[index],
  }))
}

/** The chart of an overview with buckets of `hours`. */
function chartOf(range: Range, first: string, hours: number, count: number, fill: Record<number, Partial<Bucket>> = {}) {
  return jobsChart({ range, bucket_ms: hours * 3_600_000, buckets: buckets(first, hours, count, fill) })
}

describe('jobsChart', () => {
  test('24h: hourly labels and a tick every 6 hours, as in the design', () => {
    const chart = chartOf('24h', '2026-11-03T15:00:00+09:00', 1, 24, { 20: { succeeded: 125, failed: 6 } })
    expect(chart.title).toBe('Jobs per hour')
    expect(chart.ticks).toEqual([
      { at: 0, label: '15:00' },
      { at: 6, label: '21:00' },
      { at: 12, label: '03:00' },
      { at: 18, label: '09:00' },
    ])
    expect(chart.bars[20]).toEqual({ label: '11:00 – 12:00', value: 131, part: 6, detail: '131 jobs · 6 failed' })
    expect(chart.bars[23]!.label).toBe('14:00 – 15:00')
  })

  test('7d: 6-hour buckets carry the day, and ticks fall a day apart', () => {
    const chart = chartOf('7d', '2026-10-29T06:00:00+09:00', 6, 28)
    expect(chart.title).toBe('Jobs per 6 hours')
    expect(chart.bars[0]!.label).toBe('Oct 29, 06:00 – 12:00')
    expect(chart.ticks[1]).toEqual({ at: 4, label: 'Oct 30' })
  })

  test('30d: weekly ticks stop a week before the end', () => {
    const chart = chartOf('30d', '2026-10-06T00:00:00+09:00', 24, 30)
    expect(chart.title).toBe('Jobs per day')
    expect(chart.ticks).toEqual([
      { at: 2, label: 'Oct 8' },
      { at: 9, label: 'Oct 15' },
      { at: 16, label: 'Oct 22' },
      { at: 23, label: 'Oct 29' },
    ])
    expect(chart.bars[29]!.label).toBe('Nov 4')
  })

  test('cancelled jobs count in the bar and are named in the tooltip', () => {
    const chart = chartOf('24h', '2026-11-03T15:00:00+09:00', 1, 24, { 3: { succeeded: 1, cancelled: 2 } })
    expect(chart.bars[3]).toMatchObject({ value: 3, part: 0, detail: '3 jobs · 0 failed · 2 cancelled' })
    expect(chart.bars[4]!.detail).toBe('No jobs')
    expect(chart.empty).toBe(false)
  })

  test('is empty when no bucket has a job', () => {
    expect(chartOf('7d', '2026-10-29T06:00:00+09:00', 6, 28).empty).toBe(true)
  })
})

describe('statValues', () => {
  test('formats the API fractions as percents', () => {
    const values = statValues(overview({ jobs: 1284, median_queue_ms: 2_840, failure_rate: 0.041, cache_hit_rate: 0.92 }))
    expect(values).toEqual({ jobs: '1,284', medianQueue: '2.8s', failureRate: '4.1%', cacheHitRate: '92%' })
  })

  test('shows dashes for a range without jobs', () => {
    expect(statValues(overview({ jobs: 0 }))).toEqual({ jobs: '0', medianQueue: null, failureRate: null, cacheHitRate: null })
  })

  test('tells a real 0 from no data', () => {
    const values = statValues(overview({ jobs: 2, median_queue_ms: 0, failure_rate: 0, cache_hit_rate: 0 }))
    expect(values).toMatchObject({ medianQueue: '0s', failureRate: '0%', cacheHitRate: '0%' })
  })
})

test('jobProgress is unknown without a usual run time or past it', () => {
  expect(jobProgress(30_000, 60_000)).toBe(50)
  expect(jobProgress(60_000, 60_000)).toBe(100)
  expect(jobProgress(61_000, 60_000)).toBeNull()
  expect(jobProgress(5_000, 0)).toBeNull()
})

function overview(stats: Partial<Schemas['OverviewStats']>): Overview {
  return {
    range: '24h',
    bucket_ms: 3_600_000,
    stats: {
      jobs: 0,
      vcpu_in_use: 0,
      vcpu_total: 16,
      median_queue_ms: null,
      failure_rate: null,
      cache_hit_rate: null,
      duration_ms: 0,
      ...stats,
    },
    running: [],
    servers: [],
    buckets: [],
    recent: [],
  }
}
