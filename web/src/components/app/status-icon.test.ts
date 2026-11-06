import { describe, expect, test } from 'vitest'
import { stepStatus } from './status-icon'

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
