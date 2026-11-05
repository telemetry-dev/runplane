import { $api } from './client'

/** The state of the updater of runplane. It answers 503 when this build has no updater. */
export const updateQuery = $api.queryOptions('get', '/api/update')
