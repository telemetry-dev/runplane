import { $api } from '~/api/client'

export const settingsQuery = $api.queryOptions('get', '/api/settings')

export const poolsQuery = $api.queryOptions('get', '/api/pools')
