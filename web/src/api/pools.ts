import type { QueryClient } from '@tanstack/react-query'
import { $api, type Schemas } from './client'
import type { paths } from './schema'

type Pool = Schemas['Pool']

export const poolsQuery = $api.queryOptions('get', '/api/pools')

/** The servers that could place a pool of this size. */
export function placementQuery(query: NonNullable<paths['/api/pools/placement']['get']['parameters']['query']>) {
  return $api.queryOptions('get', '/api/pools/placement', { params: { query } })
}

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

/** Writes the counts of a pool.stats event into the cached pool. */
export function putPoolStats(client: QueryClient, { pool_id, ...counts }: Schemas['PoolStats']) {
  client.setQueryData<Pool[]>(poolsQuery.queryKey, (list) => list?.map((pool) => (pool.id === pool_id ? { ...pool, ...counts } : pool)))
}
