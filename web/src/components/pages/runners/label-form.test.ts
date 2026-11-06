import { describe, expect, test } from 'vitest'
import { draftFrom, poolChanges, poolRequest, poolUpdate, type Draft } from './label-form'
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
    const draft = draftFrom(undefined, 'arm64', 'img-default')
    expect(draft).toMatchObject({ arch: 'arm64', vcpu: 4, memoryGb: 16, diskGb: 80, allServers: true, maxRunners: 10 })
  })
})

describe('poolRequest', () => {
  const draft: Draft = { ...draftFrom(pool, 'x64', undefined), label: ' runplane-16cpu ', memoryGb: 0.5 }

  test('trims the label and sends memory in MB', () => {
    expect(poolRequest(draft, serverArch)).toMatchObject({ label: 'runplane-16cpu', memory_mb: 512 })
  })

  test('is null while a number is empty', () => {
    expect(poolRequest({ ...draft, maxRunners: null }, serverArch)).toBeNull()
  })

  test('sends no servers for all servers, though some stay checked', () => {
    expect(poolRequest({ ...draft, allServers: true }, serverArch)?.server_ids).toEqual([])
  })

  test('drops checked servers of another arch', () => {
    const request = poolRequest({ ...draft, arch: 'arm64', serverIds: ['primary', 'arm'] }, serverArch)
    expect(request?.server_ids).toEqual(['arm'])
  })
})

describe('poolChanges', () => {
  const before = poolRequest(draftFrom(pool, 'x64', undefined), serverArch)!

  test('holds only the fields that changed', () => {
    expect(poolChanges(before, { ...before, label: 'runplane-32cpu', max_runners: 8 })).toEqual({ label: 'runplane-32cpu', max_runners: 8 })
  })

  test('the same servers in another order are no change', () => {
    const two = { ...before, server_ids: ['primary', 'agent'] }
    expect(poolChanges(two, { ...two, server_ids: ['agent', 'primary'] })).toEqual({})
  })

  test('removing all chosen servers sends the empty list', () => {
    expect(poolChanges(before, { ...before, server_ids: [] })).toEqual({ server_ids: [] })
  })
})

describe('poolUpdate', () => {
  test('sends only the changed fields', () => {
    const initial = draftFrom(pool, 'x64', undefined)
    const after = poolRequest({ ...initial, vcpu: 8 }, serverArch)!
    expect(poolUpdate(pool, initial, after, serverArch)).toEqual({ vcpu: 8 })
  })

  test('drops the ID of a removed server, which the API would refuse', () => {
    const stale = { ...pool, server_ids: ['primary', 'removed'] }
    const initial = draftFrom(stale, 'x64', undefined)
    const after = poolRequest({ ...initial, vcpu: 8 }, serverArch)!
    expect(poolUpdate(stale, initial, after, serverArch)).toEqual({ vcpu: 8, server_ids: ['primary'] })
  })
})
