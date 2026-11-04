import { describe, expect, test } from 'vitest'
import { currentUsage, diskDetail, lastSeen, shortDigest, sortChecks, statusDetail, type Check, type Server } from './server-format'

// A server as GET /api/servers/{id} returns it (the seeded fsn1-ax52-b).
const agent: Server = {
  id: '01m408d877nkw2kdaqn7j298qx',
  name: 'fsn1-ax52-b',
  kind: 'agent',
  status: 'online',
  status_detail: '',
  arch: 'x64',
  mode: 'vm',
  vcpu: 16,
  memory_mb: 65536,
  disk_bytes: 1099511627776,
  cpu_percent: 31,
  memory_percent: 22,
  disk_percent: 38,
  running_jobs: 1,
  sandboxes: 2,
  vcpu_in_use: 6,
  memory_in_use_mb: 24576,
  last_seen_at: '2026-11-04T04:49:39.447Z',
  address: '136.243.19.12',
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
  update_state: 'current',
  stats_at: '2026-11-04T04:49:39.447Z',
  created_at: '2026-10-03T06:49:45.447Z',
  updated_at: '2026-11-04T04:49:39.447Z',
}

const now = Date.parse('2026-11-04T04:49:51.447Z')

describe('statusDetail', () => {
  test('an agent shows when it was last seen', () => {
    expect(statusDetail(agent, agent.last_seen_at, now)).toBe('seen 12s ago')
    expect(statusDetail(agent, '2026-11-04T04:49:49.447Z', now)).toBe('seen just now')
  })

  test('the primary reports no heartbeat', () => {
    expect(statusDetail({ ...agent, kind: 'local' }, agent.last_seen_at, now)).toBe('')
  })

  test('an installing or unreachable server shows its detail, or else when it was seen', () => {
    expect(statusDetail({ ...agent, status: 'installing', status_detail: 'pulling runner image' }, null, now)).toBe('pulling runner image')
    expect(statusDetail({ ...agent, status: 'unreachable' }, '2026-11-04T02:49:51.447Z', now)).toBe('seen 2h ago')
  })
})

describe('lastSeen', () => {
  const stats = { server_id: agent.id, cpu_percent: 40, memory_percent: 25, disk_percent: 38, at: '2026-11-04T04:49:49.447Z' }

  test('a newer host sample counts as a sign of life', () => {
    expect(lastSeen(agent, stats)).toBe('2026-11-04T04:49:49.447Z')
  })

  test('an older sample does not move the time back', () => {
    expect(lastSeen(agent, { ...stats, at: '2026-11-04T04:40:00Z' })).toBe(agent.last_seen_at)
  })
})

describe('currentUsage', () => {
  const stats = { server_id: agent.id, cpu_percent: 40, memory_percent: 25, disk_percent: 39, at: '2026-11-04T04:49:49.447Z' }

  test('uses the host.stats event when it is newer than the sample', () => {
    expect(currentUsage(agent, stats)).toEqual({ cpu: 40, memory: 25, disk: 39 })
    expect(currentUsage(agent, { ...stats, at: '2026-11-04T04:40:00Z' })).toEqual({ cpu: 31, memory: 22, disk: 38 })
  })

  test('an unreachable server has no current usage', () => {
    expect(currentUsage({ ...agent, status: 'unreachable' }, stats)).toEqual({ cpu: null, memory: null, disk: null })
  })
})

describe('diskDetail', () => {
  test('one unit when both sizes share it', () => {
    expect(diskDetail({ ...agent, disk_bytes: 1030792151040 }, 27)).toBe('259 of 960 GB used · NVMe')
  })

  test('both units when they differ', () => {
    expect(diskDetail(agent, 38)).toBe('389 GB of 1 TB used · NVMe')
  })

  test('the size alone without a sample', () => {
    expect(diskDetail({ ...agent, disk_kind: 'ssd' }, null)).toBe('1 TB · SSD')
  })
})

describe('sortChecks', () => {
  test('failures, then warnings, then passed checks in the doctor order', () => {
    const check = (id: string, status: Check['status']): Check => ({ id, name: id, status, detail: '', fix: '', command: '' })
    const sorted = sortChecks([check('mode', 'ok'), check('time', 'warn'), check('kernel', 'ok'), check('nft', 'fail'), check('kvm', 'ok')])
    expect(sorted.map((item) => item.id)).toEqual(['nft', 'time', 'mode', 'kernel', 'kvm'])
  })
})

describe('shortDigest', () => {
  test('keeps the algorithm and 12 hex digits', () => {
    expect(shortDigest('sha256:218d01223e363d0bbe033fd2c36cc3b68b25015e11347f6f3608b296febc2f63')).toBe('sha256:218d01223e36')
  })
})
