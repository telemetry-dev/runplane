import type { QueryClient } from '@tanstack/react-query'
import { $api, type Schemas } from './client'
import { withHostStats } from './servers'

type Overview = Schemas['Overview']

/** The overview of a range. Without a range, its key reaches the overviews of each range. */
export function overviewQuery(range?: Overview['range']) {
  return $api.queryOptions('get', '/api/overview', range && { params: { query: { range } } })
}

/** Writes a host.stats event into the servers of each cached overview. */
export function putOverviewHostStats(client: QueryClient, stats: Schemas['HostStats']) {
  client.setQueriesData<Overview>(
    { queryKey: overviewQuery().queryKey },
    (overview) =>
      overview && {
        ...overview,
        servers: overview.servers.map((server) => (server.id === stats.server_id ? withHostStats(server, stats) : server)),
      },
  )
}
