import { $api } from './client'

/** The invite of a link, before sign-in. */
export function inviteQuery(token: string) {
  return $api.queryOptions('get', '/api/invites/{id}', { params: { path: { id: token } } }, { staleTime: Infinity })
}
