import { $api } from './client'

/** Users and open invites, for admins. */
export const membersQuery = $api.queryOptions('get', '/api/users')
