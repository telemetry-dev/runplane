import { queryOptions } from '@tanstack/react-query'
import { $api, api, isApiError } from './client'

// Queries of the app shell and the route guards. Pages define their own with $api.

export const statusQuery = $api.queryOptions('get', '/api/status')

/** The signed-in user, or null when there is no session. */
export const sessionQuery = queryOptions({
  queryKey: ['get', '/api/session'] as const,
  queryFn: async ({ signal }) => {
    try {
      const { data } = await api.GET('/api/session', { signal })
      return data ?? null
    } catch (error) {
      if (isApiError(error, 401)) return null
      throw error
    }
  },
})

export const githubQuery = $api.queryOptions('get', '/api/github')

/** Running jobs for the sidebar badge. A runplane instance has far fewer than 100 at once. */
export const runningJobsQuery = $api.queryOptions('get', '/api/jobs', {
  params: { query: { status: 'running', limit: 100 } },
})
