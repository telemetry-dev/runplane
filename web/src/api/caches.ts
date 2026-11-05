import { $api } from './client'

export const cachesQuery = $api.queryOptions('get', '/api/caches')
