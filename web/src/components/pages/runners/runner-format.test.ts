import { describe, expect, test } from 'vitest'
import type { Schemas } from '~/api/client'
import { formatTimeLimit, formatUptime, placementProblem, poolScope, sortSandboxes, type Sandbox } from './runner-format'

// A sandbox as GET /api/sandboxes returns it (a seeded busy runner).
const busy: Sandbox = {
  id: '01m6jdzf2r4a89jers7bgay6dh',
  pool_id: '01m3xfn9a715fvmxcphqcvwnpn',
  pool_label: 'runplane-4cpu-arm',
  server_id: '01m40bv3v72phnmwp5mekkqmwr',
  server_name: 'hel1-cax41',
  state: 'busy',
  runner_name: 'runplane-4cpu-arm-gay6dh',
  job_id: '01m6je8hcjjmjtmpvjwe8qaf3j',
  job_name: 'lint',
  job_repository: 'kestrel-hq/mobile',
  job_workflow_file: '.github/workflows/ci.yml',
  debug: false,
  hold_until: null,
  vcpu: 4,
  memory_mb: 8192,
  error: '',
  created_at: '2026-11-04T04:44:48.6Z',
  idle_at: '2026-11-04T04:44:53.048Z',
  busy_at: '2026-11-04T04:49:32.6Z',
  stopped_at: null,
}

function sandbox(runner_name: string, state: Sandbox['state'], created_at: string): Sandbox {
  return { ...busy, id: runner_name, runner_name, state, created_at }
}

describe('sortSandboxes', () => {
  test('starting, busy, idle, then stopping; the newest first within a state', () => {
    const sorted = sortSandboxes([
      sandbox('idle-old', 'idle', '2026-11-04T04:00:00Z'),
      sandbox('stopping', 'stopping', '2026-11-04T05:00:00Z'),
      sandbox('busy', 'busy', '2026-11-04T04:30:00Z'),
      sandbox('idle-new', 'idle', '2026-11-04T04:40:00Z'),
      sandbox('starting', 'starting', '2026-11-04T04:10:00Z'),
    ])
    expect(sorted.map((item) => item.runner_name)).toEqual(['starting', 'busy', 'idle-new', 'idle-old', 'stopping'])
  })
})

describe('formatUptime', () => {
  test.each([
    [9_000, '9s'],
    [75_000, '1m 15s'],
    [242_000, '4m 02s'],
    // From 10 minutes the seconds stop ticking.
    [10 * 60_000 + 30_000, '10m'],
    [26 * 60_000 + 59_000, '26m'],
    [65 * 60_000, '1h 05m'],
  ])('%i ms is %s', (ms, text) => {
    expect(formatUptime(ms)).toBe(text)
  })
})

describe('poolScope', () => {
  const servers = [
    { id: 'a', name: 'fsn1-ax102-a' },
    { id: 'b', name: 'fsn1-ax52-b' },
  ] as Schemas['Server'][]

  test('a pool without servers uses all servers of its arch', () => {
    expect(poolScope({ arch: 'arm64', server_ids: [] }, servers)).toBe('All arm64 servers')
  })

  test('names the chosen servers, and a server that is gone', () => {
    expect(poolScope({ arch: 'x64', server_ids: ['b', 'a', 'gone'] }, servers)).toBe('fsn1-ax52-b, fsn1-ax102-a, Removed server')
  })
})

test.each([
  [{ online_servers: 3, waiting_servers: 1, placeable_servers: 1 }, undefined],
  [{ online_servers: 0, waiting_servers: 0, placeable_servers: 0 }, 'No x64 server is online'],
  // allow_public_repos is on, and each server is in container mode.
  [{ online_servers: 2, waiting_servers: 2, placeable_servers: 0 }, 'No x64 server gets runners now'],
  [{ online_servers: 2, waiting_servers: 1, placeable_servers: 0 }, 'No x64 server is large enough'],
])('placementProblem(%j) is %j', (placement, want) => {
  expect(placementProblem('x64', placement)).toBe(want)
})

describe('formatTimeLimit', () => {
  test.each([
    [30, '30 minutes'],
    [60, '1 hour'],
    [360, '6 hours'],
    [90, '1 hour 30 minutes'],
  ])('%i minutes is %s', (minutes, text) => {
    expect(formatTimeLimit(minutes)).toBe(text)
  })
})
