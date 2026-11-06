import { $api } from './client'

export const backupsQuery = $api.queryOptions('get', '/api/backups')
