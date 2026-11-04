import { $api, type Schemas } from '~/api/client'

/** GET /api/users: users and open invites, for admins. */
export const membersQuery = $api.queryOptions('get', '/api/users')

/** A page of GET /api/audit, newest first, for admins. The first page has the cursor "". */
export function auditQuery(cursor: string) {
  return $api.queryOptions('get', '/api/audit', { params: { query: { cursor: cursor || undefined, limit: 10 } } })
}

export type Members = Schemas['Members']
export type Role = Schemas['User']['role']

export const roles: { value: Role; label: string }[] = [
  { value: 'admin', label: 'Admin' },
  { value: 'member', label: 'Member' },
]
