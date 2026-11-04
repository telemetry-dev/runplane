import type { QueryClient } from '@tanstack/react-query'
import { $api } from '~/api/client'
import type { Pool } from './runner-format'

/** GET /api/pools with the live counts. sandbox.state events refetch it. */
export const poolsQuery = $api.queryOptions('get', '/api/pools')

/** Sandboxes that are not gone or failed, of all servers or of one. sandbox.state events refetch them. */
export function sandboxesQuery(serverId?: string) {
  return $api.queryOptions('get', '/api/sandboxes', serverId ? { params: { query: { server_id: serverId } } } : {})
}

/** image.state events refetch it. */
export const imagesQuery = $api.queryOptions('get', '/api/images')

/** The host reserve and overcommit for the size hint, and the default time limit. */
export const settingsQuery = $api.queryOptions('get', '/api/settings')

/** Puts a pool from a mutation response into the cached list at once, as a new row if it is new. */
export function putPool(client: QueryClient, pool: Pool) {
  client.setQueryData<Pool[]>(poolsQuery.queryKey, (list) => {
    if (!list) return list
    return list.some((item) => item.id === pool.id) ? list.map((item) => (item.id === pool.id ? pool : item)) : [...list, pool]
  })
}

export function dropPool(client: QueryClient, id: string) {
  client.setQueryData<Pool[]>(poolsQuery.queryKey, (list) => list?.filter((item) => item.id !== id))
}

/** Refetches every list that shows pools or their sandboxes. */
export function refreshRunners(client: QueryClient) {
  return Promise.all([
    client.invalidateQueries({ queryKey: ['get', '/api/pools'] }),
    client.invalidateQueries({ queryKey: ['get', '/api/sandboxes'] }),
  ])
}
