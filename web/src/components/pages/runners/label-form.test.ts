import { describe, expect, test } from 'vitest'
import { draftFrom, poolRequest, poolUpdate, type Draft } from './label-form'
import type { Pool } from './runner-format'

// A pool as GET /api/pools returns it (the seeded runplane-16cpu).
const pool: Pool = {
  id: '01m3xfkeq70181kvyx350bgc91',
  name: 'Extra large',
  label: 'runplane-16cpu',
  arch: 'x64',
  vcpu: 16,
  memory_mb: 65536,
  disk_gb: 120,
  image_id: '',
  image_name: 'ubuntu-24.04',
  min_idle: 0,
  max_runners: 4,
  time_limit_minutes: 0,
  server_ids: ['primary'],
  online_servers: 1,
  waiting_servers: 0,
  placeable_servers: 1,
  placeable_server_ids: ['primary'],
  enabled: true,
  created_at: '2026-10-02T04:57:45.447Z',
  updated_at: '2026-10-02T04:57:45.447Z',
  idle: 0,
  busy: 0,
  starting: 0,
  queued: 1,
  assigned: 0,
}

const serverArch = new Map([
  ['primary', 'x64'],
  ['agent', 'x64'],
  ['arm', 'arm64'],
])

describe('draftFrom', () => {
  test('a pool set to the default image reads as a pool without an image', () => {
    expect(draftFrom({ ...pool, image_id: 'img-default' }, 'x64', 'img-default').imageId).toBe('')
  })

  test('a new pool uses the given arch and all servers', () => {
    expect(draftFrom(undefined, 'arm64', 'img-default')).toMatchObject({ arch: 'arm64', allServers: true })
  })
})

describe('poolRequest', () => {
  const draft: Draft = { ...draftFrom(pool, 'x64', undefined), label: ' runplane-16cpu ', memoryGb: 0.5 }

  test('trims the label and sends memory in MB', () => {
    expect(poolRequest(draft, serverArch)).toMatchObject({ label: 'runplane-16cpu', memory_mb: 512 })
  })

  test.each(['vcpu', 'memoryGb', 'diskGb', 'minIdle', 'maxRunners'] as const)('is null while %s is empty', (field) => {
    expect(poolRequest({ ...draft, [field]: null }, serverArch)).toBeNull()
  })

  test('sends no servers for all servers, though some stay checked', () => {
    expect(poolRequest({ ...draft, allServers: true }, serverArch)?.server_ids).toEqual([])
  })

  test('drops checked servers of another arch', () => {
    const request = poolRequest({ ...draft, arch: 'arm64', serverIds: ['primary', 'arm'] }, serverArch)
    expect(request?.server_ids).toEqual(['arm'])
  })
})

describe('poolUpdate', () => {
  /** The PATCH body after the user changes `edit` in the dialog of `saved`. */
  function update(saved: Pool, edit: Partial<Draft>) {
    const initial = draftFrom(saved, 'x64', undefined)
    return poolUpdate(saved, initial, poolRequest({ ...initial, ...edit }, serverArch)!, serverArch)
  }

  test('sends only the changed fields', () => {
    expect(update(pool, { vcpu: 8, label: 'runplane-32cpu', maxRunners: 8 })).toEqual({ vcpu: 8, label: 'runplane-32cpu', max_runners: 8 })
  })

  test('the same servers in another order are no change', () => {
    expect(update({ ...pool, server_ids: ['primary', 'agent'] }, { serverIds: ['agent', 'primary'] })).toEqual({})
  })

  test('a switch to all servers sends the empty list', () => {
    expect(update(pool, { allServers: true })).toEqual({ server_ids: [] })
  })

  test('drops the ID of a removed server, which the API would refuse', () => {
    expect(update({ ...pool, server_ids: ['primary', 'removed'] }, { vcpu: 8 })).toEqual({ vcpu: 8, server_ids: ['primary'] })
  })
})
