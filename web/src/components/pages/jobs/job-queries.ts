import { queryOptions } from '@tanstack/react-query'
import { $api, api } from '~/api/client'

/** Job detail. job.state events merge into this query (see ~/api/events). */
export function jobQuery(id: string) {
  return $api.queryOptions('get', '/api/jobs/{id}', { params: { path: { id } } })
}

export function metricsQuery(id: string) {
  return $api.queryOptions('get', '/api/jobs/{id}/metrics', { params: { path: { id } } })
}

/** A stored log does not change, so it stays cached. `step` 0 is the whole log. */
export function logQuery(id: string, step: number) {
  return queryOptions({
    queryKey: ['get', '/api/jobs/{id}/log', { params: { path: { id }, query: { step } } }] as const,
    queryFn: async ({ signal }) => {
      const { data } = await api.GET('/api/jobs/{id}/log', {
        params: { path: { id }, query: { step: step || undefined } },
        parseAs: 'text',
        signal,
      })
      return data ?? ''
    },
    staleTime: Infinity,
  })
}
