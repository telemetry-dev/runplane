import type { Schemas } from '~/api/client'
import type { HostStats } from '~/api/events'
import { gigabytes } from '~/components/pages/runners/runner-format'
import { formatBytes, formatRelativeTime } from '~/lib/format'

// Text of the Server detail design, from GET /api/servers/{id}, the doctor, and GET /api/images.

export type Server = Schemas['Server']
export type Check = Schemas['Check']
type Image = Schemas['Image']
type ImageBuild = Schemas['ImageBuild']

/** The state after the status word: "seen 4s ago" for an agent, or the install or error detail. */
export function statusDetail(server: Server, seenAt: string | null, now: number): string {
  const ago = seenAt ? formatRelativeTime(seenAt, now) : ''
  const seen = server.kind === 'agent' && ago ? `seen ${ago === 'now' ? 'just now' : ago}` : ''
  if (server.status === 'installing' || server.status === 'unreachable') return server.status_detail || seen
  return seen
}

/** The newer of the agent's last message and its last host sample, which also proves it is alive. */
export function lastSeen(server: Server, live: HostStats | undefined): string | null {
  if (!live) return server.last_seen_at
  if (!server.last_seen_at) return live.at
  return Date.parse(live.at) > Date.parse(server.last_seen_at) ? live.at : server.last_seen_at
}

/** "VM" with "Firecracker 1.10.1", "Container" with "Sysbox 0.6.4", or "Dev". */
export function sandboxMode(server: Server): { name: string; detail: string } | null {
  if (server.mode === 'vm') return { name: 'VM', detail: server.firecracker_version && `Firecracker ${server.firecracker_version}` }
  if (server.mode === 'container') return { name: 'Container', detail: server.sysbox_version && `Sysbox ${server.sysbox_version}` }
  if (server.mode === 'dev') return { name: 'Dev', detail: '' }
  return null
}

/** "24 of 64 GB allocated to runners" */
export function memoryDetail(server: Server): string {
  return `${gigabytes(server.memory_in_use_mb)} of ${gigabytes(server.memory_mb)} GB allocated to runners`
}

/** "259 of 960 GB used · NVMe", or "418 GB of 1 TB used · NVMe" when the units differ. */
export function diskDetail(server: Server, percent: number | null): string {
  if (!server.disk_bytes) return ''
  const total = formatBytes(server.disk_bytes)
  let used = total
  if (percent !== null) {
    const amount = formatBytes((server.disk_bytes * percent) / 100)
    const [value, unit] = amount.split(' ')
    used = `${unit === total.split(' ')[1] ? value : amount} of ${total} used`
  }
  return [used, server.disk_kind].filter(Boolean).join(' · ')
}

/** The doctor needs a server that answers and has finished its install. */
export function canRunDoctor(server: Server): boolean {
  return server.status !== 'unreachable' && server.status !== 'installing'
}

/** Problems first, failures before warnings; passed checks keep the doctor's order. */
export function sortChecks(checks: readonly Check[]): Check[] {
  return checks.toSorted((a, b) => checkRank[a.status] - checkRank[b.status])
}

/** "sha256:4be1c0d9e2a1" from a full image digest. */
export function shortDigest(digest: string): string {
  const [algorithm, hex] = digest.split(':')
  return hex ? `${algorithm}:${hex.slice(0, 12)}` : digest
}

/** The build of an image on one server, if the server has one. */
export function buildOn(image: Image, serverId: string): ImageBuild | undefined {
  return image.builds.find((build) => build.server_id === serverId)
}

const checkRank: Record<Check['status'], number> = { fail: 0, warn: 1, ok: 2 }

