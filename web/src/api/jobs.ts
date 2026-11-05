import { queryOptions } from '@tanstack/react-query'
import { $api, api } from './client'
import type { FollowedRequest } from './followed-text'
import type { paths } from './schema'

export type JobsQuery = NonNullable<paths['/api/jobs']['get']['parameters']['query']>

/** A page of jobs. Without a query, its key reaches every job list. */
export function jobsQuery(query?: JobsQuery) {
  return $api.queryOptions('get', '/api/jobs', query && { params: { query } })
}

/** The sidebar badge counts the running jobs from the total of the page. */
export const runningJobsQuery = jobsQuery({ status: 'running', limit: 1 })

export const facetsQuery = $api.queryOptions('get', '/api/jobs/facets')

export function jobQuery(id: string) {
  return $api.queryOptions('get', '/api/jobs/{id}', { params: { path: { id } } })
}

export function metricsQuery(id: string) {
  return $api.queryOptions('get', '/api/jobs/{id}/metrics', { params: { path: { id } } })
}

/** A stored log does not change, so it stays cached. `step` 0 is the whole log. */
export function logQuery(id: string, step: number) {
  const init = { params: { path: { id }, query: { step: step || undefined } }, parseAs: 'text' } as const
  return queryOptions({
    queryKey: $api.queryOptions('get', '/api/jobs/{id}/log', init).queryKey,
    // An empty log has no body.
    queryFn: async ({ signal }) => (await api.GET('/api/jobs/{id}/log', { ...init, signal })).data ?? '',
    staleTime: Infinity,
  })
}

/** The live log of a running job, for useFollowedText. */
export function followJobLog(id: string, step: number): FollowedRequest {
  return {
    key: `job ${id} ${step}`,
    open: async (signal) => {
      const { data } = await api.GET('/api/jobs/{id}/log', {
        params: { path: { id }, query: { step: step || undefined, follow: true } },
        parseAs: 'stream',
        signal,
      })
      return data
    },
  }
}
