import type { Schemas } from '~/api/client'
import { gigabytes } from '~/components/pages/runners/runner-format'
import { formatBytes, formatTimeAgo } from '~/lib/format'

// Text of the Server detail design, from GET /api/servers/{id}, the doctor, and GET /api/images.

export type Server = Schemas['Server']
type Image = Schemas['Image']
type ImageBuild = Schemas['ImageBuild']

/** The state after the status word: "seen 4s ago" for an agent, or the install or error detail. */
export function statusDetail(server: Server, now: number): string {
  const seen = server.kind === 'agent' && server.last_seen_at ? `seen ${formatTimeAgo(server.last_seen_at, now)}` : ''
  if (server.status === 'installing' || server.status === 'unreachable') return server.status_detail || seen
  return seen
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

/** "sha256:4be1c0d9e2a1" from a full image digest. */
export function shortDigest(digest: string): string {
  const [algorithm, hex] = digest.split(':')
  return hex ? `${algorithm}:${hex.slice(0, 12)}` : digest
}

/** The build of an image on one server, if the server has one. */
export function buildOn(image: Image, serverId: string): ImageBuild | undefined {
  return image.builds.find((build) => build.server_id === serverId)
}

