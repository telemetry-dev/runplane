import { describe, expect, test } from 'vitest'
import { clock, jobOrigin, noLogReason, queueTime, runTime, trigger } from './job-format'

const now = Date.parse('2026-11-04T05:00:00Z')

describe('queueTime and runTime', () => {
  test('a queued job waits until now and has no run time', () => {
    const job = { state: 'queued', queued_at: '2026-11-04T04:59:19Z', started_at: null, queue_ms: 3000, duration_ms: 0 } as const
    expect(queueTime(job, now)).toBe(41_000)
    expect(runTime(job, now)).toBeNull()
  })

  test('a running job runs until now', () => {
    const job = { state: 'running', queued_at: '2026-11-04T04:58:00Z', started_at: '2026-11-04T04:58:18Z', queue_ms: 2000, duration_ms: 100 } as const
    expect(queueTime(job, now)).toBe(2000)
    expect(runTime(job, now)).toBe(102_000)
  })

  test('a completed job keeps its recorded times', () => {
    const job = { state: 'completed', queued_at: '2026-11-04T04:38:40Z', started_at: '2026-11-04T04:38:45Z', queue_ms: 4000, duration_ms: 408_000 } as const
    expect(queueTime(job, now)).toBe(4000)
    expect(runTime(job, now)).toBe(408_000)
  })
})

describe('jobOrigin', () => {
  test('names the repository, workflow file, and run number', () => {
    expect(jobOrigin({ repository: 'kestrel-hq/api', workflow_file: '.github/workflows/ci.yml', run_number: 4182 })).toBe(
      'kestrel-hq/api · ci.yml · #4182',
    )
  })

  test('leaves out a run number that runplane has not read yet', () => {
    expect(jobOrigin({ repository: 'kestrel-hq/api', workflow_file: '.github/workflows/ci.yml', run_number: 0 })).toBe('kestrel-hq/api · ci.yml')
  })
})

describe('trigger', () => {
  test.each([
    [{ event: 'pull_request', actor: 'mira-k', pr_number: 412 }, 'PR #412 by mira-k'],
    [{ event: 'push', actor: 'ada', pr_number: 0 }, 'push by ada'],
    [{ event: 'workflow_dispatch', actor: 'ada', pr_number: 0 }, 'workflow dispatch by ada'],
    [{ event: 'schedule', actor: '', pr_number: 0 }, 'schedule'],
  ])('%o: %s', (job, text) => {
    expect(trigger(job)).toBe(text)
  })
})

describe('clock', () => {
  test.each([
    [0, '0:00'],
    [9_999, '0:09'],
    [408_000, '6:48'],
    [3_725_000, '1:02:05'],
    [-500, '0:00'],
  ])('%i ms is %s', (ms, text) => {
    expect(clock(ms)).toBe(text)
  })
})

describe('noLogReason', () => {
  test('tells a job that has not run from a finished job without a log', () => {
    const job = { state: 'queued', started_at: null, reject_reason: '' } as const
    expect(noLogReason(job)).toBe('The log starts when the job runs.')
    expect(noLogReason({ ...job, state: 'completed' })).toBe('The job ended before it ran.')
    expect(noLogReason({ ...job, state: 'completed', started_at: '2026-11-04T04:38:45Z' })).toBe('GitHub has no log for this job.')
  })

  test('gives the reason runplane refused the job', () => {
    expect(noLogReason({ state: 'completed', started_at: null, reject_reason: 'the repository is public' })).toBe(
      'runplane does not run this job: the repository is public',
    )
  })
})
