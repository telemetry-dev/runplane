import { $api } from './client'

/**
 * A page of the audit log, newest first, for admins. The first page has the
 * cursor "". Without a cursor, its key reaches every page.
 */
export function auditQuery(cursor?: string) {
  return $api.queryOptions('get', '/api/audit', cursor === undefined ? undefined : { params: { query: { cursor: cursor || undefined, limit: 10 } } })
}
