import { useQuery, useQueryClient, type QueryClient } from '@tanstack/react-query'
import { $api, type Schemas } from './client'

type Server = Schemas['Server']

export const serversQuery = $api.queryOptions('get', '/api/servers')

export function serverQuery(id: string) {
  return $api.queryOptions('get', '/api/servers/{id}', { params: { path: { id } } })
}

/** The last doctor result of a server. */
export function doctorQuery(serverId: string) {
  return $api.queryOptions('get', '/api/servers/{id}/doctor', { params: { path: { id: serverId } } })
}

export const sshKeyQuery = $api.queryOptions('get', '/api/servers/ssh-key', undefined, { staleTime: Infinity })

export function tokenStatusQuery(tokenId: string) {
  return $api.queryOptions('get', '/api/servers/tokens/{id}/status', { params: { path: { id: tokenId } } })
}

/** Puts a server into its cached detail and the cached list. Returns false if the list lacks it, as a new server. */
export function putServer(client: QueryClient, server: Server): boolean {
  client.setQueryData<Server>(serverQuery(server.id).queryKey, (old) => old && server)
  let listed = true
  client.setQueryData<Server[]>(serversQuery.queryKey, (list) => {
    if (!list) return list
    const index = list.findIndex((item) => item.id === server.id)
    if (index >= 0) return list.with(index, server)
    listed = false
    return list
  })
  return listed
}

/** Writes a host.stats event into the cached server list and detail. */
export function putHostStats(client: QueryClient, stats: Schemas['HostStats']) {
  client.setQueryData<Server>(serverQuery(stats.server_id).queryKey, (old) => old && withHostStats(old, stats))
  client.setQueryData<Server[]>(serversQuery.queryKey, (list) =>
    list?.map((server) => (server.id === stats.server_id ? withHostStats(server, stats) : server)),
  )
}

type Sampled = Pick<Server, 'kind' | 'cpu_percent' | 'memory_percent' | 'disk_percent' | 'last_seen_at'> & Partial<Pick<Server, 'stats_at'>>

/**
 * The server with the usage of a host sample, unless it holds a newer one. A
 * sample also proves that an agent is alive, so runplane records its time as
 * the agent's last contact.
 */
export function withHostStats<T extends Sampled>(server: T, stats: Schemas['HostStats']): T {
  if (server.stats_at && Date.parse(server.stats_at) > Date.parse(stats.at)) return server
  const seen = server.kind === 'agent' && (!server.last_seen_at || Date.parse(stats.at) > Date.parse(server.last_seen_at))
  return {
    ...server,
    cpu_percent: stats.cpu_percent,
    memory_percent: stats.memory_percent,
    disk_percent: stats.disk_percent,
    ...(server.stats_at !== undefined ? { stats_at: stats.at } : {}),
    ...(seen ? { last_seen_at: stats.at } : {}),
  }
}

export function putDoctor(client: QueryClient, serverId: string, result: Schemas['DoctorResult']) {
  client.setQueryData(doctorQuery(serverId).queryKey, result)
}

export type Doctor = {
  /** The last result: undefined while it loads, null before the first run. */
  result: Schemas['DoctorResult'] | null | undefined
  running: boolean
  error: unknown
  run: () => void
}

/** The stored doctor result of a server, and a run of the checks that replaces it. */
export function useDoctor(serverId: string): Doctor {
  const client = useQueryClient()
  const stored = useQuery(doctorQuery(serverId))
  const run = $api.useMutation('post', '/api/servers/{id}/doctor', {
    onSuccess: (data) => putDoctor(client, serverId, data),
  })
  return {
    result: stored.data && (stored.data.checked_at ? stored.data : null),
    running: run.isPending,
    error: run.error ?? stored.error,
    run: () => run.mutate({ params: { path: { id: serverId } } }),
  }
}
