import { describe, expect, test } from 'vitest'
import { jobStatus, stepStatus } from './status-icon'

describe('jobStatus', () => {
  test.each([
    [{ state: 'queued', result: null }, 'queued'],
    [{ state: 'assigned', result: null }, 'running'],
    [{ state: 'running', result: null }, 'running'],
    [{ state: 'completed', result: 'succeeded' }, 'succeeded'],
    [{ state: 'completed', result: 'cancelled' }, 'cancelled'],
    [{ state: 'completed', result: 'skipped' }, 'skipped'],
  ])('%o shows as %s', (job, status) => {
    expect(jobStatus(job)).toBe(status)
  })
})

describe('stepStatus', () => {
  test.each([
    [{ status: 'queued', conclusion: '' }, 'queued'],
    [{ status: 'in_progress', conclusion: '' }, 'running'],
    [{ status: 'completed', conclusion: 'success' }, 'succeeded'],
    [{ status: 'completed', conclusion: 'failure' }, 'failed'],
    [{ status: 'completed', conclusion: 'skipped' }, 'skipped'],
    [{ status: 'completed', conclusion: 'cancelled' }, 'cancelled'],
    [{ status: 'completed', conclusion: 'timed_out' }, 'failed'],
  ])('%o shows as %s', (step, status) => {
    expect(stepStatus(step)).toBe(status)
  })
})
