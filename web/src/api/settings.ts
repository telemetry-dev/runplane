import { useQueryClient } from '@tanstack/react-query'
import { $api, type Schemas } from './client'
import { statusQuery } from './session'

type Settings = Schemas['Settings']

/** Every user can read the settings; only admins can change them. */
export const settingsQuery = $api.queryOptions('get', '/api/settings')

/** The DNS record of the domain and the addresses of this server. */
export const domainDNSQuery = $api.queryOptions('get', '/api/settings/dns')

/** The settings keys that a Switch row changes. */
export type SwitchKey = 'auto_update' | 'backup_enabled' | 'actions_cache' | 'registry_mirror'

/**
 * PATCH /api/settings for one control, so each control has its own pending
 * state and error. The response replaces the cached settings. With
 * `optimistic`, the cache takes the new switch values at once and gets the
 * old ones back if the API refuses them.
 */
export function useSaveSettings({ optimistic = false } = {}) {
  const client = useQueryClient()
  return $api.useMutation('patch', '/api/settings', {
    onMutate: async ({ body }) => {
      if (!optimistic) return undefined
      await client.cancelQueries({ queryKey: settingsQuery.queryKey })
      const previous = client.getQueryData<Settings>(settingsQuery.queryKey)
      client.setQueryData<Settings>(settingsQuery.queryKey, (old) => old && { ...old, ...(body as Partial<Pick<Settings, SwitchKey>>) })
      return { previous }
    },
    onError: (_error, { body }, context) => {
      const previous = context?.previous
      if (!previous) return
      // Put back only this control's keys; other controls may have saved since.
      const keys = Object.keys(body) as SwitchKey[]
      client.setQueryData<Settings>(
        settingsQuery.queryKey,
        (old) => old && { ...old, ...Object.fromEntries(keys.map((key) => [key, previous[key]])) },
      )
    },
    onSuccess: (saved, { body }) => {
      client.setQueryData<Settings>(settingsQuery.queryKey, saved)
      // The sidebar shows the instance name from GET /api/status.
      if (body.instance_name !== undefined) void client.invalidateQueries({ queryKey: statusQuery.queryKey })
      if (body.domain !== undefined) void client.invalidateQueries({ queryKey: domainDNSQuery.queryKey })
    },
  })
}
