import type { QueryClient } from '@tanstack/react-query'
import { $api } from '~/api/client'
import type { Server } from './server-info'

/** GET /api/servers. server.state events update it, and sandbox.state events refetch it. */
export const serversQuery = $api.queryOptions('get', '/api/servers')

/**
 * Puts a server from a mutation response into the cached list at once, then
 * refetches the lists that show servers.
 */
export function updateServer(client: QueryClient, server: Server) {
  client.setQueryData<Server[]>(serversQuery.queryKey, (list) => list?.map((item) => (item.id === server.id ? server : item)))
  return refreshServers(client)
}

export function dropServer(client: QueryClient, id: string) {
  client.setQueryData<Server[]>(serversQuery.queryKey, (list) => list?.filter((item) => item.id !== id))
  return refreshServers(client)
}

export function refreshServers(client: QueryClient) {
  return Promise.all([
    client.invalidateQueries({ queryKey: ['get', '/api/servers'] }),
    client.invalidateQueries({ queryKey: ['get', '/api/overview'] }),
  ])
}
