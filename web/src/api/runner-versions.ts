import { $api } from './client'

export const runnerVersionsQuery = $api.queryOptions('get', '/api/runner-versions')
