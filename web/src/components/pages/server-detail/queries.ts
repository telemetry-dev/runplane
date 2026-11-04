import { useQuery, useQueryClient, type QueryClient } from '@tanstack/react-query'
import { $api, type Schemas } from '~/api/client'
import { updateServer } from '~/components/pages/servers/queries'
import type { Server } from './server-format'

/** GET /api/servers/{id}. server.state events merge into it. */
export function serverQuery(id: string) {
  return $api.queryOptions('get', '/api/servers/{id}', { params: { path: { id } } })
}

/** Puts a server from a PATCH response into the detail and the lists at once. */
export function putServer(client: QueryClient, server: Server) {
  client.setQueryData(serverQuery(server.id).queryKey, server)
  return updateServer(client, server)
}

/** GET /api/servers/{id}/doctor: the last doctor result of the server. */
export function doctorQuery(serverId: string) {
  return $api.queryOptions('get', '/api/servers/{id}/doctor', { params: { path: { id: serverId } } })
}

export type Doctor = {
  /** The last result: undefined while it loads, null before the first run. */
  result: Schemas['DoctorResult'] | null | undefined
  running: boolean
  error: unknown
  run: () => void
}

/** The stored doctor result of a server, and a run of the checks that replaces it. */
export function useDoctor(serverId: string): Doctor {
  const client = useQueryClient()
  const stored = useQuery(doctorQuery(serverId))
  const run = $api.useMutation('post', '/api/servers/{id}/doctor', {
    onSuccess: (data) => client.setQueryData(doctorQuery(serverId).queryKey, data),
  })
  return {
    result: stored.data && (stored.data.checked_at ? stored.data : null),
    running: run.isPending,
    error: run.error ?? stored.error,
    run: () => run.mutate({ params: { path: { id: serverId } } }),
  }
}
