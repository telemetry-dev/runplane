import { $api, api } from './client'
import type { FollowedRequest } from './followed-text'

/** The sandboxes that are not gone or failed, of one server or of all. The key of all of them reaches each list. */
export function sandboxesQuery(serverId?: string) {
  return $api.queryOptions('get', '/api/sandboxes', serverId ? { params: { query: { server_id: serverId } } } : undefined)
}

export function sandboxQuery(id: string) {
  return $api.queryOptions('get', '/api/sandboxes/{id}', { params: { path: { id } } })
}

/** The log of a runner, for useFollowedText. It streams until the runner stops. */
export function followSandboxLog(id: string): FollowedRequest {
  return {
    key: `sandbox ${id}`,
    open: async (signal) => {
      const { data } = await api.GET('/api/sandboxes/{id}/log', { params: { path: { id }, query: { follow: true } }, parseAs: 'stream', signal })
      return data
    },
  }
}
