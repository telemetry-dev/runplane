import { $api } from './client'

/** The notification channels, for admins. */
export const notificationsQuery = $api.queryOptions('get', '/api/notifications')
