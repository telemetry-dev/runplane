import { describe, expect, test, vi } from 'vitest'
import type { Schemas } from '~/api/client'
import { hasNoJobs, jobProgress, jobsChart, statValues, type Overview } from './data'

// Chart labels use local time; pin it before data.ts makes its formatter.
vi.hoisted(() => {
  process.env.TZ = 'UTC'
})

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

describe('jobsChart', () => {
  test('24h: hourly labels and a tick every 6 hours, as in the design', () => {
    const chart = jobsChart('24h', buckets('2026-11-03T15:00:00Z', 1, 24, { 20: { succeeded: 125, failed: 6 } }))
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
    const chart = jobsChart('7d', buckets('2026-10-29T06:00:00Z', 6, 28))
    expect(chart.bars[0]!.label).toBe('Oct 29, 06:00 – 12:00')
    expect(chart.ticks[1]).toEqual({ at: 4, label: 'Oct 30' })
  })

  test('30d: weekly ticks stop a week before the end', () => {
    const chart = jobsChart('30d', buckets('2026-10-06T00:00:00Z', 24, 30))
    expect(chart.ticks).toEqual([
      { at: 2, label: 'Oct 8' },
      { at: 9, label: 'Oct 15' },
      { at: 16, label: 'Oct 22' },
      { at: 23, label: 'Oct 29' },
    ])
    expect(chart.bars[29]!.label).toBe('Nov 4')
  })

  test('cancelled jobs count in the bar and are named in the tooltip', () => {
    const chart = jobsChart('24h', buckets('2026-11-03T15:00:00Z', 1, 24, { 3: { succeeded: 1, cancelled: 2 } }))
    expect(chart.bars[3]).toMatchObject({ value: 3, part: 0, detail: '3 jobs · 0 failed · 2 cancelled' })
    expect(chart.bars[4]!.detail).toBe('No jobs')
    expect(chart.empty).toBe(false)
  })

  test('is empty when no bucket has a job', () => {
    expect(jobsChart('7d', buckets('2026-10-29T06:00:00Z', 6, 28)).empty).toBe(true)
  })
})

describe('statValues', () => {
  test('formats the API fractions as percents', () => {
    const values = statValues(overview({ jobs: 1284, median_queue_ms: 2_840, failure_rate: 0.041, cache_hit_rate: 0.92 }, { succeeded: 9, failed: 1 }))
    expect(values).toEqual({ jobs: '1,284', medianQueue: '2.8s', failureRate: '4.1%', cacheHitRate: '92%' })
  })

  test('shows no failure rate while no job has succeeded or failed', () => {
    const values = statValues(overview({ jobs: 3, failure_rate: null }, { cancelled: 3 }))
    expect(values.failureRate).toBeNull()
  })

  test('shows dashes for a range without jobs', () => {
    expect(statValues(overview({ jobs: 0 }))).toEqual({ jobs: '0', medianQueue: null, failureRate: null, cacheHitRate: null })
  })

  test('tells a real 0 from no data', () => {
    const values = statValues(overview({ jobs: 2, median_queue_ms: 0, cache_hit_rate: 0 }))
    expect(values).toMatchObject({ medianQueue: '0s', cacheHitRate: '0%' })
  })
})

test('jobProgress is unknown without a usual run time or past it', () => {
  expect(jobProgress(30_000, 60_000)).toBe(50)
  expect(jobProgress(60_000, 60_000)).toBe(100)
  expect(jobProgress(61_000, 60_000)).toBeNull()
  expect(jobProgress(5_000, 0)).toBeNull()
})

describe('hasNoJobs', () => {
  test('is true before the first job', () => {
    expect(hasNoJobs(overview({ jobs: 0 }))).toBe(true)
  })

  test('is false when older jobs exist but the range has none', () => {
    expect(hasNoJobs({ ...overview({ jobs: 0 }), recent: [job] })).toBe(false)
  })

  test('is false while the first job waits or runs', () => {
    expect(hasNoJobs(overview({ jobs: 1 }))).toBe(false)
    expect(hasNoJobs({ ...overview({ jobs: 0 }), running: [{ ...job, state: 'running', result: null, finished_at: null }] })).toBe(false)
  })
})

function overview(stats: Partial<Schemas['OverviewStats']>, bucket: Partial<Bucket> = {}): Overview {
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
    buckets: buckets('2026-11-03T15:00:00Z', 1, 24, { 23: bucket }),
    recent: [],
  }
}

const job: Schemas['JobSummary'] = {
  id: '01m6je8hchzdf5e8fshs2jfy2f',
  job_name: 'test (node 22)',
  workflow_name: 'CI',
  workflow_file: '.github/workflows/ci.yml',
  repository: 'kestrel-hq/api',
  run_number: 4181,
  head_branch: 'main',
  head_sha: '1d0e7aa90c44391443d4b9ca016410176d990cbb',
  pool_id: '01m3xfhm47vd7m44bm5jv7gn1y',
  pool_label: 'runplane-8cpu',
  server_id: '01m3xf4sz7v9b1yvym3hxcaey9',
  server_name: 'fsn1-ax102-a',
  state: 'completed',
  result: 'succeeded',
  debug: false,
  reject_reason: '',
  queue_ms: 2000,
  duration_ms: 192000,
  queued_at: '2026-11-04T04:44:42.6Z',
  started_at: '2026-11-04T04:44:45Z',
  finished_at: '2026-11-04T04:47:57Z',
  expected_duration_ms: 0,
}
