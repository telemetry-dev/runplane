import { describe, expect, test } from 'vitest'
import type { Schemas } from '~/api/client'
import { defaultStep, longestStep, parseJobSearch, shownStep, withoutRunGroups } from './job-steps'

function step(number: number, conclusion: string, started_at: string | null, completed_at: string | null): Schemas['JobStep'] {
  return {
    number,
    name: `Step ${number}`,
    status: completed_at ? 'completed' : started_at ? 'in_progress' : 'queued',
    conclusion,
    started_at,
    completed_at,
  }
}

const steps = [
  step(1, 'success', '2026-11-04T04:38:47Z', '2026-11-04T04:38:49Z'),
  step(2, 'cancelled', '2026-11-04T04:38:49Z', '2026-11-04T04:38:52Z'),
  step(3, 'failure', '2026-11-04T04:38:52Z', '2026-11-04T04:44:24Z'),
  step(4, 'failure', '2026-11-04T04:44:24Z', '2026-11-04T04:44:25Z'),
  step(5, 'skipped', '2026-11-04T04:44:25Z', '2026-11-04T04:44:25Z'),
]

describe('defaultStep', () => {
  test('opens the first failed step', () => {
    expect(defaultStep(steps)).toBe(3)
  })

  test('opens the whole log when no step failed', () => {
    expect(defaultStep([steps[0]!, steps[1]!, steps[4]!])).toBe(0)
    expect(defaultStep([])).toBe(0)
  })
})

describe('shownStep', () => {
  test('a stored log shows the step in the URL, or else the first failed step', () => {
    expect(shownStep({ log_state: 'stored', steps }, 1)).toBe(1)
    expect(shownStep({ log_state: 'stored', steps }, 0)).toBe(0)
    expect(shownStep({ log_state: 'stored', steps }, undefined)).toBe(3)
  })

  test('a live log shows whole, because the stream has every step', () => {
    expect(shownStep({ log_state: 'live', steps }, 1)).toBe(0)
    expect(shownStep({ log_state: 'live', steps }, undefined)).toBe(0)
  })
})

describe('longestStep', () => {
  test('picks the step that ran longest', () => {
    expect(longestStep(steps, Date.parse('2026-11-04T05:00:00Z'))?.number).toBe(3)
  })

  test('counts a running step up to now and skips steps that have not started', () => {
    const running = [
      step(1, 'success', '2026-11-04T04:00:00Z', '2026-11-04T04:01:00Z'),
      step(2, '', '2026-11-04T04:01:00Z', null),
      step(3, '', null, null),
    ]
    expect(longestStep(running, Date.parse('2026-11-04T04:01:30Z'))?.number).toBe(1)
    expect(longestStep(running, Date.parse('2026-11-04T04:03:00Z'))?.number).toBe(2)
    expect(longestStep([step(1, '', null, null)], 0)).toBeUndefined()
  })
})

describe('withoutRunGroups', () => {
  test('drops the "Run" section of a step and keeps the other groups', () => {
    const log = [
      '2026-11-04T04:39:56.0000000Z ##[group]Run pnpm test:e2e --project=webkit',
      '2026-11-04T04:39:56.0010000Z pnpm test:e2e --project=webkit',
      '2026-11-04T04:39:56.0020000Z shell: /usr/bin/bash -e {0}',
      '2026-11-04T04:39:56.0030000Z ##[endgroup]',
      '2026-11-04T04:40:09.8333333Z > web@0.0.0 test:e2e /home/runner/work/web/web',
      '2026-11-04T04:40:10.0000000Z ##[group]Operating System',
      '2026-11-04T04:40:10.1000000Z Ubuntu',
      '2026-11-04T04:40:10.2000000Z ##[endgroup]',
      '2026-11-04T04:45:14.1666666Z ##[error]Process completed with exit code 1.',
    ].join('\n')
    expect(withoutRunGroups(log)).toBe(
      [
        '2026-11-04T04:40:09.8333333Z > web@0.0.0 test:e2e /home/runner/work/web/web',
        '2026-11-04T04:40:10.0000000Z ##[group]Operating System',
        '2026-11-04T04:40:10.1000000Z Ubuntu',
        '2026-11-04T04:40:10.2000000Z ##[endgroup]',
        '2026-11-04T04:45:14.1666666Z ##[error]Process completed with exit code 1.',
      ].join('\n'),
    )
  })

  test('hides a "Run" section that a live log has not closed yet', () => {
    expect(withoutRunGroups('##[group]Run make\nmake\nshell: bash')).toBe('')
  })

  test('keeps a line that only mentions a group marker', () => {
    expect(withoutRunGroups('echo "##[group]Run x"')).toBe('echo "##[group]Run x"')
  })
})

describe('parseJobSearch', () => {
  test.each([
    [{ step: 7 }, 7],
    [{ step: '7' }, 7],
    [{ step: 0 }, 0],
    [{ step: -1 }, undefined],
    [{ step: 2.5 }, undefined],
    [{ step: 'logs' }, undefined],
    [{}, undefined],
  ])('%o selects step %s', (search, expected) => {
    expect(parseJobSearch(search).step).toBe(expected)
  })
})
