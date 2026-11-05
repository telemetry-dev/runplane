import { $api } from './client'

/** The check of the server that runs the setup, for a setup code. */
export function setupHostQuery(code: string) {
  return $api.queryOptions('get', '/api/setup/host', { params: { query: { code } } })
}
