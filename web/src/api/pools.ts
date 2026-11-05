import type { QueryClient } from '@tanstack/react-query'
import { $api, type Schemas } from './client'
import { sandboxesQuery } from './sandboxes'
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

/** Refetches the pools and their runners after a change of a pool, which starts or stops runners. */
export function refreshRunners(client: QueryClient) {
  return Promise.all([
    client.invalidateQueries({ queryKey: poolsQuery.queryKey }),
    client.invalidateQueries({ queryKey: sandboxesQuery().queryKey }),
  ])
}

/** Writes the counts of a pool.stats event into the cached pool. */
export function putPoolStats(client: QueryClient, stats: Schemas['PoolStats']) {
  const { idle, busy, starting, queued } = stats
  client.setQueryData<Pool[]>(poolsQuery.queryKey, (list) =>
    list?.map((pool) => (pool.id === stats.pool_id ? { ...pool, idle, busy, starting, queued } : pool)),
  )
}
