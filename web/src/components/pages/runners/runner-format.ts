import type { Schemas } from '~/api/client'
import type { PoolStats } from '~/api/events'
import { formatDuration } from '~/lib/format'

// Text and order of the Labels and Live runners tables, from GET /api/pools and GET /api/sandboxes.

export type Pool = Schemas['Pool']
export type Sandbox = Schemas['Sandbox']
type Server = Schemas['Server']

/** Starting runners first, then busy, idle, and stopping; the newest first within each state. */
export function sortSandboxes(sandboxes: readonly Sandbox[]): Sandbox[] {
  return sandboxes.toSorted(
    (a, b) => stateRank[a.state] - stateRank[b.state] || Date.parse(b.created_at) - Date.parse(a.created_at),
  )
}

export const stateNames: Record<Sandbox['state'], string> = {
  starting: 'Starting',
  idle: 'Idle',
  busy: 'Busy',
  stopping: 'Stopping',
  gone: 'Gone',
  failed: 'Failed',
}

/**
 * Time since a runner started: "9s" and "4m 02s" while the seconds still tell
 * something, then whole minutes ("26m") and hours ("2h 05m").
 */
export function formatUptime(ms: number): string {
  if (ms >= 10 * 60_000 && ms < 60 * 60_000) return `${Math.floor(ms / 60_000)}m`
  return formatDuration(ms)
}

/** "All x64 servers", or the names of the servers that the pool may use. */
export function poolScope(pool: Pick<Pool, 'arch' | 'server_ids'>, servers: readonly Server[] | undefined): string {
  if (pool.server_ids.length === 0) return `All ${pool.arch} servers`
  const names = new Map(servers?.map((server) => [server.id, server.name]))
  return pool.server_ids.map((id) => names.get(id) ?? 'Removed server').join(', ')
}

/** "16 vCPU · 64 GB" */
export function poolSize(pool: Pick<Pool, 'vcpu' | 'memory_mb'>): string {
  return `${pool.vcpu} vCPU · ${gigabytes(pool.memory_mb)} GB`
}

/** "x64 · 200 GB disk" */
export function poolHardware(pool: Pick<Pool, 'arch' | 'disk_gb'>): string {
  return `${pool.arch} · ${pool.disk_gb} GB disk`
}

/** Memory in GB with at most one decimal: 16384 → 16, 512 → 0.5. */
export function gigabytes(mb: number): number {
  return Math.round((mb / 1024) * 10) / 10
}

type Counts = Pick<Pool, 'idle' | 'busy' | 'starting' | 'queued'>

/** The pool counts of the last pool.stats event, or of the list until an event arrives. */
export function liveCounts(pool: Pool, live: PoolStats | undefined): Counts {
  const source = live ?? pool
  return { idle: source.idle, busy: source.busy, starting: source.starting, queued: source.queued }
}

/** "30 minutes", "1 hour", "6 hours", "1 hour 30 minutes". */
export function formatTimeLimit(minutes: number): string {
  const hours = Math.floor(minutes / 60)
  const rest = minutes % 60
  const parts = []
  if (hours > 0) parts.push(hours === 1 ? '1 hour' : `${hours} hours`)
  if (rest > 0) parts.push(rest === 1 ? '1 minute' : `${rest} minutes`)
  return parts.join(' ')
}

const stateRank: Record<Sandbox['state'], number> = { starting: 0, busy: 1, idle: 2, stopping: 3, gone: 4, failed: 4 }
