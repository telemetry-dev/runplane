import type { Schemas } from '~/api/client'
import type { Pool } from './runner-format'

// The values of the label dialog and the requests it sends.

export type Arch = Pool['arch']

/** The values of the form. Numbers are null while their input is empty; memory is in GB. */
export type Draft = {
  label: string
  arch: Arch
  vcpu: number | null
  memoryGb: number | null
  diskGb: number | null
  /** Empty: the default image. */
  imageId: string
  minIdle: number | null
  maxRunners: number | null
  /** 0: the default_time_limit_minutes setting. */
  timeLimit: number
  allServers: boolean
  /** Checked servers. Kept while "All servers" is on, so a switch back restores them. */
  serverIds: string[]
}

export type PoolRequest = Required<Omit<Schemas['CreatePoolRequest'], 'name' | 'enabled'>>

/** The values of an existing pool, or the defaults of a new one on the given arch. */
export function draftFrom(pool: Pool | undefined, arch: Arch, defaultImageId: string | undefined): Draft {
  if (!pool) {
    return {
      label: '',
      arch,
      vcpu: 4,
      memoryGb: 16,
      diskGb: 80,
      imageId: '',
      minIdle: 0,
      maxRunners: 10,
      timeLimit: 0,
      allServers: true,
      serverIds: [],
    }
  }
  return {
    label: pool.label,
    arch: pool.arch,
    vcpu: pool.vcpu,
    memoryGb: pool.memory_mb / 1024,
    diskGb: pool.disk_gb,
    // A pool set to the default image reads like a pool without an image.
    imageId: pool.image_id === defaultImageId ? '' : pool.image_id,
    minIdle: pool.min_idle,
    maxRunners: pool.max_runners,
    timeLimit: pool.time_limit_minutes,
    allServers: pool.server_ids.length === 0,
    serverIds: pool.server_ids,
  }
}

/**
 * The request body of a draft, or null while a number is empty. Only the checked
 * servers of the draft's arch count.
 */
export function poolRequest(draft: Draft, serverArch: ReadonlyMap<string, string>): PoolRequest | null {
  const { vcpu, memoryGb, diskGb, minIdle, maxRunners } = draft
  if (vcpu === null || memoryGb === null || diskGb === null || minIdle === null || maxRunners === null) return null
  return {
    label: draft.label.trim(),
    arch: draft.arch,
    vcpu,
    memory_mb: Math.round(memoryGb * 1024),
    disk_gb: diskGb,
    image_id: draft.imageId,
    min_idle: minIdle,
    max_runners: maxRunners,
    time_limit_minutes: draft.timeLimit,
    server_ids: draft.allServers ? [] : draft.serverIds.filter((id) => serverArch.get(id) === draft.arch),
  }
}

/**
 * The PATCH body of an edit: the fields of `after` that differ from the pool.
 * The servers compare with the pool's stored IDs, which may name removed
 * servers that the draft leaves out, so a save drops those IDs. The API
 * refuses a pool that names a removed server.
 */
export function poolUpdate(pool: Pool, initial: Draft, after: PoolRequest, serverArch: ReadonlyMap<string, string>): Schemas['UpdatePoolRequest'] {
  const before = poolRequest(initial, serverArch)
  return before ? poolChanges({ ...before, server_ids: pool.server_ids }, after) : after
}

/** The fields of `after` that differ from `before`, for PATCH /api/pools/{id}. */
export function poolChanges(before: PoolRequest, after: PoolRequest): Schemas['UpdatePoolRequest'] {
  const changes: Record<string, unknown> = {}
  for (const key of Object.keys(after) as (keyof PoolRequest)[]) {
    if (!sameValue(before[key], after[key])) changes[key] = after[key]
  }
  return changes
}

function sameValue(a: unknown, b: unknown): boolean {
  if (Array.isArray(a) && Array.isArray(b)) return a.toSorted().join() === b.toSorted().join()
  return a === b
}

/**
 * The body fields that the dialog shows: the Fields of the same name, and the
 * servers and the image, whose problems show under those controls.
 */
export const formFields = ['label', 'vcpu', 'memory_mb', 'disk_gb', 'min_idle', 'max_runners', 'server_ids', 'image_id']
