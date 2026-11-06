import { QueryClient, QueryObserver, type QueryKey } from '@tanstack/react-query'
import { afterEach, beforeEach, describe, expect, test, vi } from 'vitest'
import { backupsQuery } from './backups'
import { cachesQuery } from './caches'
import { createEventHandler, reconnectDelay } from './events'
import { githubQuery } from './github'
import { imagesQuery } from './images'
import { jobQuery, jobsQuery, metricsQuery } from './jobs'
import { overviewQuery } from './overview'
import { poolsQuery } from './pools'
import { sandboxesQuery, sandboxQuery } from './sandboxes'
import { serverQuery, serversQuery } from './servers'
import { statusQuery } from './session'
import { domainDNSQuery, settingsQuery } from './settings'

const jobKey = (id: string) => jobQuery(id).queryKey
const metricsKey = (id: string) => metricsQuery(id).queryKey
const jobListKey = jobsQuery({ status: 'running' }).queryKey
const serverListKey = serversQuery.queryKey
const serverKey = (id: string) => serverQuery(id).queryKey
const overviewKey = overviewQuery('24h').queryKey

describe('createEventHandler', () => {
  let client: QueryClient
  const invalidated = (key: QueryKey) => client.getQueryState(key)?.isInvalidated

  beforeEach(() => {
    vi.useFakeTimers()
    client = new QueryClient()
  })

  afterEach(() => {
    client.clear()
    vi.useRealTimers()
  })

  test('job.state merges into the cached job and refetches only that job, its metrics, job lists, and the status', async () => {
    client.setQueryData(jobKey('j1'), { id: 'j1', state: 'running', steps: [{ name: 'Checkout' }] })
    client.setQueryData(jobKey('j2'), { id: 'j2', state: 'queued' })
    client.setQueryData(metricsKey('j1'), { samples: [] })
    client.setQueryData(metricsKey('j2'), { samples: [] })
    client.setQueryData(jobListKey, { items: [] })
    client.setQueryData(serverListKey, [])
    client.setQueryData(statusQuery.queryKey, { has_jobs: false })
    const handle = createEventHandler(client)

    handle('job.state', { id: 'j1', state: 'completed', result: 'failed' })

    expect(client.getQueryData(jobKey('j1'))).toEqual({
      id: 'j1',
      state: 'completed',
      result: 'failed',
      steps: [{ name: 'Checkout' }],
    })
    expect(client.getQueryData(jobKey('j2'))).toEqual({ id: 'j2', state: 'queued' })

    await vi.advanceTimersByTimeAsync(300)
    expect(invalidated(jobKey('j1'))).toBe(true)
    expect(invalidated(metricsKey('j1'))).toBe(true)
    expect(invalidated(jobListKey)).toBe(true)
    // The first-job step of the setup waits for has_jobs.
    expect(invalidated(statusQuery.queryKey)).toBe(true)
    expect(invalidated(jobKey('j2'))).toBe(false)
    expect(invalidated(metricsKey('j2'))).toBe(false)
    expect(invalidated(serverListKey)).toBe(false)
  })

  test('job.state keeps the status after the first job, because has_jobs stays true', async () => {
    client.setQueryData(statusQuery.queryKey, { has_jobs: true })
    const handle = createEventHandler(client)

    handle('job.state', { id: 'j1', state: 'running' })
    await vi.advanceTimersByTimeAsync(300)

    expect(invalidated(statusQuery.queryKey)).toBe(false)
  })

  test('sandbox.state refetches that sandbox, the server list and its server, and the job it runs', async () => {
    const sandboxKey = (id: string) => sandboxQuery(id).queryKey
    for (const key of [sandboxKey('sb1'), sandboxKey('sb2'), serverKey('s1'), serverKey('s2'), jobKey('j1'), jobKey('j2')]) {
      client.setQueryData(key, {})
    }
    client.setQueryData(sandboxesQuery().queryKey, [])
    client.setQueryData(sandboxesQuery('s1').queryKey, [])
    client.setQueryData(serverListKey, [])
    const handle = createEventHandler(client)

    handle('sandbox.state', stoppedDebugSandbox)
    await vi.advanceTimersByTimeAsync(300)

    expect(invalidated(sandboxesQuery().queryKey)).toBe(true)
    expect(invalidated(sandboxesQuery('s1').queryKey)).toBe(true)
    expect(invalidated(serverListKey)).toBe(true)
    expect(invalidated(sandboxKey('sb1'))).toBe(true)
    expect(invalidated(serverKey('s1'))).toBe(true)
    expect(invalidated(jobKey('j1'))).toBe(true)
    expect(invalidated(sandboxKey('sb2'))).toBe(false)
    expect(invalidated(serverKey('s2'))).toBe(false)
    expect(invalidated(jobKey('j2'))).toBe(false)
  })

  test('server.state puts the server into its detail and the server list, and refetches the lists that count servers', async () => {
    const other = { ...drainingServer, id: 's2', name: 'fsn1-ax52-b', status: 'online', update_state: 'current' }
    client.setQueryData(serverKey('s1'), { ...drainingServer, status: 'online', update_state: 'available' })
    client.setQueryData(serverKey('s2'), other)
    client.setQueryData(serverListKey, [{ ...drainingServer, status: 'online', update_state: 'available' }, other])
    client.setQueryData(poolsQuery.queryKey, [])
    client.setQueryData(overviewKey, {})
    client.setQueryData(jobsQuery().queryKey, { items: [] })
    const handle = createEventHandler(client)

    handle('server.state', drainingServer)

    expect(client.getQueryData(serverKey('s1'))).toEqual(drainingServer)
    expect(client.getQueryData(serverKey('s2'))).toEqual(other)
    expect(client.getQueryData(serverListKey)).toEqual([drainingServer, other])
    await vi.advanceTimersByTimeAsync(300)
    expect(invalidated(serverListKey)).toBe(false)
    expect(invalidated(serverKey('s1'))).toBe(false)
    expect(invalidated(poolsQuery.queryKey)).toBe(true)
    expect(invalidated(overviewKey)).toBe(true)
    expect(invalidated(jobsQuery().queryKey)).toBe(false)
  })

  test('server.state of a server that the list lacks refetches the list', async () => {
    client.setQueryData(serverListKey, [{ ...drainingServer, id: 's2', name: 'fsn1-ax52-b' }])
    const handle = createEventHandler(client)

    handle('server.state', { ...drainingServer, id: 's3', name: 'gra-b2-30', status: 'installing' })

    await vi.advanceTimersByTimeAsync(300)
    expect(client.getQueryState(serverListKey)?.isInvalidated).toBe(true)
  })

  test('server.removed drops the server from the list, and refetches its detail and the lists that count servers', async () => {
    const other = { ...drainingServer, id: 's2', name: 'fsn1-ax52-b' }
    client.setQueryData(serverListKey, [drainingServer, other])
    client.setQueryData(serverKey('s1'), drainingServer)
    client.setQueryData(serverKey('s2'), other)
    client.setQueryData(poolsQuery.queryKey, [])
    client.setQueryData(overviewKey, {})
    const handle = createEventHandler(client)

    handle('server.removed', { id: 's1' })

    expect(client.getQueryData(serverListKey)).toEqual([other])
    await vi.advanceTimersByTimeAsync(300)
    // An open Server detail then shows that the server is gone.
    expect(invalidated(serverKey('s1'))).toBe(true)
    expect(invalidated(serverKey('s2'))).toBe(false)
    expect(invalidated(poolsQuery.queryKey)).toBe(true)
    expect(invalidated(overviewKey)).toBe(true)
  })

  test('settings.changed replaces the cached settings, and refetches the queries that show settings', async () => {
    client.setQueryData(settingsQuery.queryKey, { instance_name: 'runplane' })
    for (const key of [statusQuery.queryKey, domainDNSQuery.queryKey, cachesQuery.queryKey, poolsQuery.queryKey, serverListKey]) {
      client.setQueryData(key, {})
    }
    const handle = createEventHandler(client)

    handle('settings.changed', { instance_name: 'Kestrel CI' })

    expect(client.getQueryData(settingsQuery.queryKey)).toEqual({ instance_name: 'Kestrel CI' })
    await vi.advanceTimersByTimeAsync(300)
    expect(invalidated(settingsQuery.queryKey)).toBe(false)
    // The sidebar shows the name from the status.
    expect(invalidated(statusQuery.queryKey)).toBe(true)
    expect(invalidated(domainDNSQuery.queryKey)).toBe(true)
    expect(invalidated(cachesQuery.queryKey)).toBe(true)
    expect(invalidated(poolsQuery.queryKey)).toBe(true)
    expect(invalidated(serverListKey)).toBe(false)
  })

  test('github.state replaces the cached GitHub status and backup.state refetches the backups', async () => {
    const status = { connected: true, installations: [{ id: 7, account_login: 'kestrel-hq' }], scale_sets: [] }
    client.setQueryData(githubQuery.queryKey, { connected: false, installations: [], scale_sets: [] })
    client.setQueryData(backupsQuery.queryKey, [])
    client.setQueryData(serverListKey, [])
    const handle = createEventHandler(client)

    handle('github.state', status)
    handle('backup.state', { id: 'bk1', status: 'running' })

    expect(client.getQueryData(githubQuery.queryKey)).toEqual(status)
    await vi.advanceTimersByTimeAsync(300)
    expect(client.getQueryState(backupsQuery.queryKey)?.isInvalidated).toBe(true)
    expect(client.getQueryState(githubQuery.queryKey)?.isInvalidated).toBe(false)
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

    const handle = createEventHandler(client)
    for (let i = 0; i < 20; i++) handle('job.state', { id: `j${i}`, state: 'running' })
    await vi.advanceTimersByTimeAsync(300)

    expect(fetches).toBe(2)
    unsubscribe()
  })

  test('pool.stats moves the counts of its pool in the cached list', () => {
    const counts = { idle: 0, busy: 0, starting: 1, queued: 0, assigned: 0 }
    client.setQueryData(poolsQuery.queryKey, [
      { id: 'p1', label: 'runplane-4cpu', ...counts },
      { id: 'p2', label: 'runplane-8cpu', ...counts },
    ])
    const handle = createEventHandler(client)

    handle('pool.stats', { pool_id: 'p1', idle: 1, busy: 3, starting: 0, queued: 2, assigned: 1 })

    expect(client.getQueryData(poolsQuery.queryKey)).toEqual([
      { id: 'p1', label: 'runplane-4cpu', idle: 1, busy: 3, starting: 0, queued: 2, assigned: 1 },
      { id: 'p2', label: 'runplane-8cpu', ...counts },
    ])
  })

  test('pools.changed refetches the pools and the images, which count the pools that use them', async () => {
    client.setQueryData(poolsQuery.queryKey, [])
    client.setQueryData(imagesQuery.queryKey, [])
    client.setQueryData(serverListKey, [])
    const handle = createEventHandler(client)

    handle('pools.changed', {})
    await vi.advanceTimersByTimeAsync(300)

    expect(invalidated(poolsQuery.queryKey)).toBe(true)
    expect(invalidated(imagesQuery.queryKey)).toBe(true)
    expect(invalidated(serverListKey)).toBe(false)
  })

  test('image.removed drops the image from the cached list', () => {
    client.setQueryData(imagesQuery.queryKey, [{ id: 'i1', name: 'ubuntu-24.04' }, { id: 'i2', name: 'android-sdk' }])
    const handle = createEventHandler(client)

    handle('image.removed', { id: 'i2' })

    expect(client.getQueryData(imagesQuery.queryKey)).toEqual([{ id: 'i1', name: 'ubuntu-24.04' }])
  })

  describe('host.stats', () => {
    // A sample 10 seconds after the one of drainingServer.
    const sample = { server_id: 's1', cpu_percent: 71, memory_percent: 40.5, disk_percent: 35, at: '2026-11-04T12:00:10Z' }
    const sampled = { cpu_percent: 71, memory_percent: 40.5, disk_percent: 35, stats_at: '2026-11-04T12:00:10Z', last_seen_at: '2026-11-04T12:00:10Z' }
    const other = { ...drainingServer, id: 's2', name: 'fsn1-ax52-b' }

    test('moves the usage of its server in the list, the detail, and the overview', () => {
      const summary = { id: 's1', kind: 'agent', cpu_percent: 88, memory_percent: 61, disk_percent: 41, stats_at: '2026-11-04T12:00:00Z', last_seen_at: null }
      client.setQueryData(serverListKey, [drainingServer, other])
      client.setQueryData(serverKey('s1'), drainingServer)
      client.setQueryData(overviewKey, { servers: [summary] })
      const handle = createEventHandler(client)

      handle('host.stats', sample)

      expect(client.getQueryData(serverListKey)).toEqual([{ ...drainingServer, ...sampled }, other])
      expect(client.getQueryData(serverKey('s1'))).toEqual({ ...drainingServer, ...sampled })
      expect(client.getQueryData(overviewKey)).toEqual({ servers: [{ ...summary, ...sampled }] })
    })

    test('keeps a refetched sample that is newer than the event, in the list and in the overview', () => {
      const refetched = { ...drainingServer, stats_at: '2026-11-04T12:00:20Z' }
      const summary = { id: 's1', kind: 'agent', cpu_percent: 12, memory_percent: 30, disk_percent: 41, stats_at: '2026-11-04T12:00:20Z', last_seen_at: null }
      client.setQueryData(serverListKey, [refetched])
      client.setQueryData(overviewKey, { servers: [summary] })
      const handle = createEventHandler(client)

      handle('host.stats', sample)

      expect(client.getQueryData(serverListKey)).toEqual([refetched])
      expect(client.getQueryData(overviewKey)).toEqual({ servers: [summary] })
    })

    test('a sample of a server without one sets its usage', () => {
      const fresh = { ...drainingServer, cpu_percent: null, memory_percent: null, disk_percent: null, stats_at: null }
      client.setQueryData(serverListKey, [fresh])
      const handle = createEventHandler(client)

      handle('host.stats', sample)

      expect(client.getQueryData(serverListKey)).toEqual([{ ...fresh, ...sampled }])
    })

    test('a sample counts as a sign of life of an agent, and an older one does not move the time back', () => {
      const seen = { ...drainingServer, stats_at: null, last_seen_at: '2026-11-04T12:00:15Z' }
      client.setQueryData(serverListKey, [seen, { ...seen, id: 's2', kind: 'local', last_seen_at: null }])
      const handle = createEventHandler(client)

      handle('host.stats', sample)
      handle('host.stats', { ...sample, server_id: 's2' })

      expect(client.getQueryData<{ last_seen_at: string | null }[]>(serverListKey)?.map((server) => server.last_seen_at)).toEqual([
        '2026-11-04T12:00:15Z',
        null,
      ])
    })
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
