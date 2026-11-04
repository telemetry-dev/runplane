import { QueryClient, QueryObserver } from '@tanstack/react-query'
import { afterEach, beforeEach, describe, expect, test, vi } from 'vitest'
import { createEventHandler, reconnectDelay } from './events'

const jobKey = (id: string) => ['get', '/api/jobs/{id}', { params: { path: { id } } }]
const metricsKey = (id: string) => ['get', '/api/jobs/{id}/metrics', { params: { path: { id } } }]
const jobListKey = ['get', '/api/jobs', { params: { query: { status: 'running' } } }]
const serverListKey = ['get', '/api/servers']

describe('createEventHandler', () => {
  let client: QueryClient

  beforeEach(() => {
    vi.useFakeTimers()
    client = new QueryClient()
  })

  afterEach(() => {
    client.clear()
    vi.useRealTimers()
  })

  test('job.state merges into the cached job and refetches only that job, its metrics, and job lists', async () => {
    client.setQueryData(jobKey('j1'), { id: 'j1', state: 'running', steps: [{ name: 'Checkout' }] })
    client.setQueryData(jobKey('j2'), { id: 'j2', state: 'queued' })
    client.setQueryData(metricsKey('j1'), { samples: [] })
    client.setQueryData(metricsKey('j2'), { samples: [] })
    client.setQueryData(jobListKey, { items: [] })
    client.setQueryData(serverListKey, [])
    const handle = createEventHandler(client, 300)

    handle('job.state', { id: 'j1', state: 'completed', result: 'failed' })

    expect(client.getQueryData(jobKey('j1'))).toEqual({
      id: 'j1',
      state: 'completed',
      result: 'failed',
      steps: [{ name: 'Checkout' }],
    })
    expect(client.getQueryData(jobKey('j2'))).toEqual({ id: 'j2', state: 'queued' })

    await vi.advanceTimersByTimeAsync(300)
    const invalidated = (key: unknown[]) => client.getQueryState(key)?.isInvalidated
    expect(invalidated(jobKey('j1'))).toBe(true)
    expect(invalidated(metricsKey('j1'))).toBe(true)
    expect(invalidated(jobListKey)).toBe(true)
    expect(invalidated(jobKey('j2'))).toBe(false)
    expect(invalidated(metricsKey('j2'))).toBe(false)
    expect(invalidated(serverListKey)).toBe(false)
  })

  test('sandbox.state refetches that sandbox, the server list and its server, and the job it runs', async () => {
    const sandboxKey = (id: string) => ['get', '/api/sandboxes/{id}', { params: { path: { id } } }]
    const serverKey = (id: string) => ['get', '/api/servers/{id}', { params: { path: { id } } }]
    for (const key of [sandboxKey('sb1'), sandboxKey('sb2'), serverKey('s1'), serverKey('s2'), jobKey('j1'), jobKey('j2')]) {
      client.setQueryData(key, {})
    }
    client.setQueryData(['get', '/api/sandboxes'], [])
    client.setQueryData(serverListKey, [])
    const handle = createEventHandler(client, 300)

    handle('sandbox.state', stoppedDebugSandbox)
    await vi.advanceTimersByTimeAsync(300)

    const invalidated = (key: unknown[]) => client.getQueryState(key)?.isInvalidated
    expect(invalidated(['get', '/api/sandboxes'])).toBe(true)
    expect(invalidated(serverListKey)).toBe(true)
    expect(invalidated(sandboxKey('sb1'))).toBe(true)
    expect(invalidated(serverKey('s1'))).toBe(true)
    expect(invalidated(jobKey('j1'))).toBe(true)
    expect(invalidated(sandboxKey('sb2'))).toBe(false)
    expect(invalidated(serverKey('s2'))).toBe(false)
    expect(invalidated(jobKey('j2'))).toBe(false)
  })

  test('server.state puts the server into its detail and the server list, and refetches the lists that count servers', async () => {
    const serverKey = (id: string) => ['get', '/api/servers/{id}', { params: { path: { id } } }]
    const other = { ...drainingServer, id: 's2', name: 'fsn1-ax52-b', status: 'online', update_state: 'current' }
    client.setQueryData(serverKey('s1'), { ...drainingServer, status: 'online', update_state: 'available' })
    client.setQueryData(serverKey('s2'), other)
    client.setQueryData(serverListKey, [{ ...drainingServer, status: 'online', update_state: 'available' }, other])
    client.setQueryData(['get', '/api/pools'], [])
    client.setQueryData(['get', '/api/overview', { params: { query: { range: '24h' } } }], {})
    client.setQueryData(['get', '/api/jobs'], { items: [] })
    const handle = createEventHandler(client, 300)

    handle('server.state', drainingServer)

    expect(client.getQueryData(serverKey('s1'))).toEqual(drainingServer)
    expect(client.getQueryData(serverKey('s2'))).toEqual(other)
    expect(client.getQueryData(serverListKey)).toEqual([drainingServer, other])
    await vi.advanceTimersByTimeAsync(300)
    const invalidated = (key: unknown[]) => client.getQueryState(key)?.isInvalidated
    expect(invalidated(serverListKey)).toBe(false)
    expect(invalidated(serverKey('s1'))).toBe(false)
    expect(invalidated(['get', '/api/pools'])).toBe(true)
    expect(invalidated(['get', '/api/overview', { params: { query: { range: '24h' } } }])).toBe(true)
    expect(invalidated(['get', '/api/jobs'])).toBe(false)
  })

  test('server.state of a server that the list lacks refetches the list', async () => {
    client.setQueryData(serverListKey, [{ ...drainingServer, id: 's2', name: 'fsn1-ax52-b' }])
    const handle = createEventHandler(client, 300)

    handle('server.state', { ...drainingServer, id: 's3', name: 'gra-b2-30', status: 'installing' })

    await vi.advanceTimersByTimeAsync(300)
    expect(client.getQueryState(serverListKey)?.isInvalidated).toBe(true)
  })

  test('github.state replaces the cached GitHub status and backup.state refetches the backups', async () => {
    const status = { connected: true, installations: [{ id: 7, account_login: 'kestrel-hq' }], scale_sets: [] }
    client.setQueryData(['get', '/api/github'], { connected: false, installations: [], scale_sets: [] })
    client.setQueryData(['get', '/api/backups'], [])
    client.setQueryData(serverListKey, [])
    const handle = createEventHandler(client, 300)

    handle('github.state', status)
    handle('backup.state', { id: 'bk1', status: 'running' })

    expect(client.getQueryData(['get', '/api/github'])).toEqual(status)
    await vi.advanceTimersByTimeAsync(300)
    expect(client.getQueryState(['get', '/api/backups'])?.isInvalidated).toBe(true)
    expect(client.getQueryState(['get', '/api/github'])?.isInvalidated).toBe(false)
    expect(client.getQueryState(serverListKey)?.isInvalidated).toBe(false)
  })

  test('a burst of events refetches an active list once', async () => {
    let fetches = 0
    const observer = new QueryObserver(client, {
      queryKey: jobListKey,
      queryFn: async () => ({ items: [], fetch: ++fetches }),
      staleTime: Infinity,
    })
    const unsubscribe = observer.subscribe(() => {})
    await vi.advanceTimersByTimeAsync(0)
    expect(fetches).toBe(1)

    const handle = createEventHandler(client, 300)
    for (let i = 0; i < 20; i++) handle('job.state', { id: `j${i}`, state: 'running' })
    await vi.advanceTimersByTimeAsync(300)

    expect(fetches).toBe(2)
    unsubscribe()
  })

  test('stats events are stored under the pool or server they name', () => {
    const handle = createEventHandler(client)
    const pool = { pool_id: 'p1', idle: 1, busy: 3, starting: 0, queued: 2, assigned: 0 }
    const host = { server_id: 's1', cpu_percent: 52, memory_percent: 40, disk_percent: 12, at: '2026-11-04T12:00:00Z' }

    handle('pool.stats', pool)
    handle('host.stats', host)

    expect(client.getQueryData(['live', 'pool.stats', 'p1'])).toEqual(pool)
    expect(client.getQueryData(['live', 'host.stats', 's1'])).toEqual(host)
  })
})

// A server.state payload (the Server API object) of an agent that an admin drains.
const drainingServer = {
  id: 's1',
  name: 'fsn1-ax52-a',
  kind: 'agent',
  status: 'draining',
  status_detail: '',
  arch: 'x64',
  mode: 'vm',
  vcpu: 16,
  memory_mb: 65536,
  disk_bytes: 1_000_000_000_000,
  cpu_percent: 88,
  memory_percent: 61,
  disk_percent: 41,
  running_jobs: 3,
  sandboxes: 4,
  vcpu_in_use: 14,
  memory_in_use_mb: 49152,
  last_seen_at: '2026-11-04T12:00:00Z',
  address: '136.243.18.77',
  provider: 'Hetzner',
  region: 'fsn1',
  os: 'Ubuntu 24.04',
  kernel: '6.8.0-87-generic',
  disk_kind: 'NVMe',
  kvm: true,
  docker_version: '28.5.1',
  sysbox_version: '',
  firecracker_version: '1.13.1',
  version: 'v0.4.2',
  update_state: 'updating',
  stats_at: '2026-11-04T12:00:00Z',
  created_at: '2026-10-02T11:14:00Z',
  updated_at: '2026-11-04T12:00:00Z',
}

// A sandbox.state payload (the Sandbox API object) of a debug sandbox stopped before its hold time.
const stoppedDebugSandbox = {
  id: 'sb1',
  state: 'gone',
  debug: true,
  error: '',
  hold_until: '2026-11-04T12:30:00Z',
  created_at: '2026-11-04T12:00:00Z',
  idle_at: '2026-11-04T12:00:20Z',
  busy_at: '2026-11-04T12:00:21Z',
  stopped_at: '2026-11-04T12:10:00Z',
  ip: '10.64.0.12',
  job_id: 'j1',
  job_name: 'test (node 22)',
  job_repository: 'kestrel-hq/api',
  job_workflow_file: '.github/workflows/ci.yml',
  memory_mb: 16384,
  vcpu: 4,
  pool_id: 'p1',
  pool_label: 'runplane-4cpu',
  runner_name: 'runplane-4cpu-7q2m',
  server_id: 's1',
  server_name: 'fsn1-ax102-a',
}

test('reconnectDelay doubles from 1s and stops at 30s', () => {
  expect([0, 1, 2, 4, 5, 12].map(reconnectDelay)).toEqual([1_000, 2_000, 4_000, 16_000, 30_000, 30_000])
})
