import { expect, test } from 'vitest'
import type { Schemas } from '~/api/client'
import { buildLabel, buildSummary, groupVersions } from './runner-info'

function build(status: Schemas['ImageBuild']['status'], server = 'fsn1-ax52-a'): Schemas['ImageBuild'] {
  return {
    server_id: server,
    server_name: server,
    arch: 'x64',
    digest: '',
    status,
    error: '',
    size_bytes: 0,
    updated_at: '2026-11-03T02:49:45.447Z',
  }
}

function bundle(fields: Partial<Schemas['RunnerVersion']>): Schemas['RunnerVersion'] {
  return {
    version: '2.340.0',
    arch: 'x64',
    status: 'ready',
    patched: true,
    sha256: '374f39fa',
    error: '',
    end_date: null,
    ends_soon: false,
    released_at: '2026-10-27T04:49:45.447Z',
    created_at: '2026-10-27T05:29:45.447Z',
    ...fields,
  }
}

test('the servers summary counts states in a fixed order, pending as waiting', () => {
  const builds = [build('pending'), build('ready'), build('failed'), build('ready'), build('pulling')]
  expect(buildSummary(builds)).toBe('2 ready · 1 pulling · 1 waiting · 1 failed')
  expect(buildSummary([])).toBe('')
  expect(buildLabel('pending')).toBe('Waiting')
})

const both = { x64: '2.340.0', arm64: '2.340.0' }

test('bundles of one version make one row, x64 first', () => {
  const [row, ...rest] = groupVersions([bundle({ arch: 'arm64' }), bundle({ arch: 'x64' })], both)
  expect(rest).toEqual([])
  expect(row).toMatchObject({ version: '2.340.0', architectures: 'x64, arm64', status: 'succeeded', inUse: 'In use' })
})

test('a failed architecture fails the version and names its error', () => {
  const [row] = groupVersions([bundle({ arch: 'x64' }), bundle({ arch: 'arm64', status: 'failed', error: 'checksum mismatch' })], {
    x64: '2.340.0',
    arm64: '',
  })
  expect(row).toMatchObject({ status: 'failed', error: 'arm64: checksum mismatch', inUse: 'In use' })
})

test('while the newest version downloads, the older ready one stays in use', () => {
  const rows = groupVersions(
    [bundle({ version: '2.341.0', status: 'pending' }), bundle({ version: '2.340.0', end_date: '2026-11-12T00:00:00Z', ends_soon: true })],
    { x64: '2.340.0', arm64: '' },
  )
  expect(rows.map(({ version, status, inUse }) => ({ version, status, inUse }))).toEqual([
    { version: '2.341.0', status: 'running', inUse: null },
    { version: '2.340.0', status: 'succeeded', inUse: 'In use' },
  ])
  expect(rows[1]).toMatchObject({ end_date: '2026-11-12T00:00:00Z', ends_soon: true })
})

test('a version that only some architectures use names them', () => {
  const rows = groupVersions(
    [
      bundle({ version: '2.341.0' }),
      bundle({ version: '2.341.0', arch: 'arm64', status: 'failed' }),
      bundle({ version: '2.340.0', arch: 'arm64' }),
    ],
    { x64: '2.341.0', arm64: '2.340.0' },
  )
  expect(rows.map((row) => row.inUse)).toEqual(['In use · x64', 'In use · arm64'])
})
