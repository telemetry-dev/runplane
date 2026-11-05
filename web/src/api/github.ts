import { useQueryClient, type QueryClient } from '@tanstack/react-query'
import { $api, type Schemas } from './client'

export const githubQuery = $api.queryOptions('get', '/api/github')

export function putGitHub(client: QueryClient, status: Schemas['GitHubStatus']) {
  client.setQueryData(githubQuery.queryKey, status)
}

/** POST /api/github/sync. runplane also syncs the installations itself; this picks up the changes made on GitHub now. */
export function useSyncGitHub(onSynced?: (status: Schemas['GitHubStatus']) => void) {
  const client = useQueryClient()
  return $api.useMutation('post', '/api/github/sync', {
    onSuccess: (status) => {
      putGitHub(client, status)
      onSynced?.(status)
    },
  })
}
