import { describe, expect, test } from 'vitest'
import {
  commandLines,
  currentUsage,
  installState,
  jobCount,
  serverHardware,
  serverOrigin,
  serverSize,
  sortServers,
  statusNote,
  systemsText,
  type Server,
} from './server-info'

// A server as GET /api/servers returns it (the seeded primary).
const primary: Server = {
  id: '01m3xf4sz7v9b1yvym3hxcaey9',
  name: 'fsn1-ax102-a',
  kind: 'local',
  status: 'online',
  status_detail: '',
  arch: 'x64',
  mode: 'vm',
  vcpu: 32,
  memory_mb: 131072,
  disk_bytes: 2089072092774,
  cpu_percent: 52,
  memory_percent: 38,
  disk_percent: 34,
  running_jobs: 3,
  sandboxes: 4,
  vcpu_in_use: 10,
  memory_in_use_mb: 40960,
  last_seen_at: '2026-11-04T04:49:42.447Z',
  address: '136.243.18.40',
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
  update_error: '',
  actions: { can_drain: true, can_resume: false, can_remove: false, can_update: false, can_run_doctor: true },
  stats_at: '2026-11-04T04:49:42.447Z',
  created_at: '2026-10-02T04:49:45.447Z',
  updated_at: '2026-11-04T04:49:42.447Z',
}

// A new agent before it reports any facts.
const fresh: Server = {
  ...primary,
  id: 'new',
  name: 'gra-b2-31',
  kind: 'agent',
  status: 'installing',
  status_detail: 'connecting over SSH',
  arch: '',
  mode: '',
  vcpu: 0,
  memory_mb: 0,
  disk_bytes: 0,
  disk_kind: '',
  address: '',
  provider: '',
  region: '',
  os: '',
  cpu_percent: null,
  memory_percent: null,
  disk_percent: null,
  running_jobs: 0,
  sandboxes: 0,
  last_seen_at: null,
  stats_at: null,
}

describe('row text', () => {
  test('joins the reported facts', () => {
    expect(serverOrigin(primary)).toBe('136.243.18.40 · Hetzner FSN1 · Ubuntu 24.04')
    expect(serverSize(primary)).toBe('32 vCPU · 128 GB')
    expect(serverHardware(primary)).toBe('x64 · 1.9 TB NVMe')
  })

  test('leaves out facts a new server has not reported', () => {
    expect(serverOrigin({ ...fresh, address: '51.68.42.111' })).toBe('51.68.42.111')
    expect(serverSize(fresh)).toBe('')
    expect(serverHardware(fresh)).toBe('')
    expect(serverHardware({ ...fresh, arch: 'x64' })).toBe('x64')
  })
})

test('jobCount names a server without running jobs Idle, as the Overview does', () => {
  expect([0, 1, 3].map(jobCount)).toEqual(['Idle', '1 job', '3 jobs'])
})

test('sortServers keeps working servers first, then installing, then unreachable, each by name', () => {
  const servers: Server[] = [
    { ...primary, name: 'ash-dedi-01', status: 'unreachable' },
    { ...primary, name: 'hel1-cax41' },
    { ...primary, name: 'gra-b2-30', status: 'installing' },
    { ...primary, name: 'fsn1-ax52-b', status: 'draining' },
    { ...primary, name: 'fsn1-ax102-a' },
    { ...primary, name: 'fsn1-ax52-a', status: 'disabled' },
  ]
  expect(sortServers(servers).map((server) => server.name)).toEqual([
    'fsn1-ax102-a',
    'fsn1-ax52-a',
    'fsn1-ax52-b',
    'hel1-cax41',
    'gra-b2-30',
    'ash-dedi-01',
  ])
})

describe('currentUsage', () => {
  test('takes the latest sample of the server', () => {
    expect(currentUsage(primary)).toEqual({ cpu: 52, memory: 38, disk: 34 })
    expect(currentUsage(fresh)).toEqual({ cpu: null, memory: null, disk: null })
  })

  test('an unreachable server has no current usage', () => {
    expect(currentUsage({ ...primary, status: 'unreachable' })).toEqual({ cpu: null, memory: null, disk: null })
  })
})

describe('statusNote', () => {
  const now = Date.parse('2026-11-04T04:49:45Z')

  test('shows the install step of an installing server', () => {
    expect(statusNote({ ...fresh, status_detail: 'pulling runner image' }, now)?.detail).toBe('pulling runner image')
  })

  test('shows the last heartbeat of an unreachable server, or its error when it never connected', () => {
    expect(statusNote({ ...primary, status: 'unreachable', last_seen_at: '2026-11-04T02:49:45Z' }, now)?.detail).toBe(
      'last heartbeat 2h ago',
    )
    expect(statusNote({ ...fresh, status: 'unreachable', status_detail: 'ssh: handshake failed' }, now)?.detail).toBe(
      'ssh: handshake failed',
    )
  })

  test('has no note for servers that show their usage', () => {
    for (const status of ['online', 'draining', 'disabled'] as const) expect(statusNote({ ...primary, status }, now)).toBeUndefined()
  })
})

test.each([
  ['no server yet', undefined, 'waiting'],
  ['installing over SSH', fresh, 'installing'],
  ['agent sent its first message', { ...fresh, last_seen_at: '2026-11-04T04:50:00Z' }, 'connected'],
  ['online without a message time', { ...fresh, status: 'online' }, 'connected'],
  ['install failed', { ...fresh, status: 'unreachable' }, 'failed'],
  ['connected, then lost', { ...primary, status: 'unreachable' }, 'failed'],
] as const)('installState: %s is %s', (_, server, want) => {
  expect(installState(server)).toBe(want)
})

describe('commandLines', () => {
  test('breaks before the token with a line continuation', () => {
    expect(commandLines('curl -fsSL https://ci.kestrel.dev/install.sh | sudo sh -s -- --token rp_srv_8Fq2nL7xVb3kR9tW')).toEqual([
      'curl -fsSL https://ci.kestrel.dev/install.sh | sudo sh -s -- \\',
      '  --token rp_srv_8Fq2nL7xVb3kR9tW',
    ])
  })

  test('puts each option of the script on its own line', () => {
    expect(
      commandLines('curl -fsSL https://ci.kestrel.dev/install.sh | sudo sh -s -- --token rp_srv_TK6U --primary ci.kestrel.dev:8485 --fingerprint sha256:f9c7'),
    ).toEqual([
      'curl -fsSL https://ci.kestrel.dev/install.sh | sudo sh -s -- \\',
      '  --token rp_srv_TK6U \\',
      '  --primary ci.kestrel.dev:8485 \\',
      '  --fingerprint sha256:f9c7',
    ])
  })

  test('keeps a command without script options on one line', () => {
    expect(commandLines('curl -fsSL https://ci.kestrel.dev/install.sh | sudo sh')).toEqual([
      'curl -fsSL https://ci.kestrel.dev/install.sh | sudo sh',
    ])
  })
})

test('systemsText groups the versions of each system and names the best-effort systems', () => {
  const systems = [
    { name: 'Ubuntu', version: '22.04', best_effort: false },
    { name: 'Ubuntu', version: '24.04', best_effort: false },
    { name: 'Debian', version: '12', best_effort: true },
    { name: 'Debian', version: '13', best_effort: true },
  ]
  expect(systemsText(systems)).toBe('Ubuntu 22.04 or 24.04, or Debian 12 or 13 (best effort)')
  expect(systemsText(systems.slice(1, 2))).toBe('Ubuntu 24.04')
})
