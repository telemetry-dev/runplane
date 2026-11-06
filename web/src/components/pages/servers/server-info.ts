import type { Schemas } from '~/api/client'
import { formatBytes, formatMegabytes, formatRelativeTime } from '~/lib/format'

// Text and state of the server rows, from the objects of GET /api/servers.

export type Server = Schemas['Server']

/** The systems that install.sh names for a server, as "Ubuntu 22.04 or 24.04, or Debian 12 or 13 (best effort)". */
export function systemsText(systems: Schemas['System'][]): string {
  const byName = new Map<string, Schemas['System'][]>()
  for (const system of systems) byName.set(system.name, [...(byName.get(system.name) ?? []), system])
  return [...byName]
    .map(([name, list]) => {
      const versions = `${name} ${list.map((system) => system.version).join(' or ')}`
      return list.every((system) => system.best_effort) ? `${versions} (best effort)` : versions
    })
    .join(', or ')
}

/** "136.243.18.40 · Hetzner FSN1 · Ubuntu 24.04", without the facts the server has not reported. */
export function serverOrigin(server: Server): string {
  return dotted([server.address, serverLocation(server), server.os])
}

/** The provider and its region, as "Hetzner FSN1". */
export function serverLocation(server: Server): string {
  return [server.provider, server.region.toUpperCase()].filter(Boolean).join(' ')
}

/** "32 vCPU · 128 GB", or empty before the server reports its facts. */
export function serverSize(server: Server): string {
  return server.vcpu ? dotted([`${server.vcpu} vCPU`, formatMegabytes(server.memory_mb)]) : ''
}

/** "x64 · 1.9 TB NVMe" */
export function serverHardware(server: Server): string {
  const disk = server.disk_bytes ? [formatBytes(server.disk_bytes), server.disk_kind].filter(Boolean).join(' ') : ''
  return dotted([server.arch, disk])
}

/** The running jobs of a server: "Idle", "1 job", "3 jobs". */
export function jobCount(count: number): string {
  if (count === 0) return 'Idle'
  return count === 1 ? '1 job' : `${count} jobs`
}

/** The short state of an agent that does not run the release of the primary: "Update available". */
export function agentUpdateNote(server: Server): string | undefined {
  return agentUpdateNotes[server.update_state]
}

const agentUpdateNotes: Partial<Record<Server['update_state'], string>> = {
  available: 'Update available',
  updating: 'Updating',
  failed: 'Update failed',
}

/** Working servers first, then installing, then unreachable; by name within each group, as in the design. */
export function sortServers(servers: readonly Server[]): Server[] {
  return servers.toSorted((a, b) => statusRank[a.status] - statusRank[b.status] || a.name.localeCompare(b.name))
}

type Usage = { cpu: number | null; memory: number | null; disk: number | null }

/** Usage percents of the latest host sample. An unreachable server's last sample is old, so its usage is unknown. */
export function currentUsage(server: Server): Usage {
  if (server.status === 'unreachable') return { cpu: null, memory: null, disk: null }
  return { cpu: server.cpu_percent, memory: server.memory_percent, disk: server.disk_percent }
}

/**
 * The text in place of the usage columns of a server that runs no jobs:
 * "Installing runner agent · pulling runner image" or "Unreachable · last heartbeat 2h ago".
 */
export function statusNote(server: Server, now = Date.now()): { title: string; detail: string } | undefined {
  if (server.status === 'installing') return { title: 'Installing runner agent', detail: server.status_detail }
  if (server.status === 'unreachable') {
    const detail = server.last_seen_at ? `last heartbeat ${formatRelativeTime(server.last_seen_at, now)}` : server.status_detail
    return { title: 'Unreachable', detail }
  }
  return undefined
}

export type InstallState = 'waiting' | 'installing' | 'connected' | 'failed'

/**
 * Progress of a server that the Add server dialog started. The agent has
 * connected once it sends a message (last_seen_at) or the server leaves
 * installing. A failed install leaves the server unreachable.
 */
export function installState(server: Server | undefined): InstallState {
  if (!server) return 'waiting'
  if (server.status === 'unreachable') return 'failed'
  if (server.status !== 'installing' || server.last_seen_at) return 'connected'
  return 'installing'
}

/**
 * Breaks the install command before each option of the script with a shell
 * line continuation, so it fits the dialog: "curl … | sudo sh -s -- \" over
 * "  --token rp_srv_… \" and the other options.
 */
export function commandLines(command: string): string[] {
  const at = command.indexOf(' -- ')
  if (at < 0) return [command]
  const lines = [command.slice(0, at + 3), ...command.slice(at + 4).split(/ (?=--)/).map((option) => `  ${option}`)]
  return lines.map((line, index) => (index < lines.length - 1 ? `${line} \\` : line))
}

export function removeDescription(runningJobs: number): string {
  if (runningJobs === 0) return 'runplane stops its sandboxes and disconnects the agent.'
  const jobs = runningJobs === 1 ? 'Its running job is' : `Its ${runningJobs} running jobs are`
  return `${jobs} cancelled, and runplane disconnects the agent.`
}

const statusRank: Record<Server['status'], number> = { online: 0, draining: 0, disabled: 0, installing: 1, unreachable: 2 }

function dotted(parts: string[]): string {
  return parts.filter(Boolean).join(' · ')
}
