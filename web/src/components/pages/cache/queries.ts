import { $api } from '~/api/client'

/**
 * GET /api/caches. No event reports cache use, so the page refetches it each
 * 30 seconds while it is open.
 */
export const cacheQuery = $api.queryOptions('get', '/api/caches', {}, { refetchInterval: 30_000 })
