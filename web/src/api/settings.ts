import { useQueryClient } from '@tanstack/react-query'
import { $api, api, type Schemas } from './client'
import { useOptimisticPatch } from './optimistic-patch'
import { statusQuery } from './session'

type Settings = Schemas['Settings']
type SettingsPatch = Schemas['SettingsPatch']

/** Every user can read the settings; only admins can change them. */
export const settingsQuery = $api.queryOptions('get', '/api/settings')

/** The DNS record of the domain and the addresses of this server. */
export const domainDNSQuery = $api.queryOptions('get', '/api/settings/dns')

/** The settings keys that a Switch changes: the settings that are on or off. */
export type SwitchKey = { [K in keyof SettingsPatch]-?: NonNullable<SettingsPatch[K]> extends boolean ? K : never }[keyof SettingsPatch]

/**
 * PATCH /api/settings for one control, so each control has its own pending
 * state and error. The response replaces the cached settings.
 */
export function useSaveSettings() {
  const client = useQueryClient()
  return $api.useMutation('patch', '/api/settings', {
    onSuccess: (saved, { body }) => {
      client.setQueryData<Settings>(settingsQuery.queryKey, saved)
      // The sidebar shows the instance name from GET /api/status.
      if (body.instance_name !== undefined) void client.invalidateQueries({ queryKey: statusQuery.queryKey })
      if (body.domain !== undefined) void client.invalidateQueries({ queryKey: domainDNSQuery.queryKey })
    },
  })
}

/** PATCH /api/settings for a switch. The cache takes the new value at once and gets the old one back if the API refuses it. */
export function useSaveSwitch(key: SwitchKey) {
  const client = useQueryClient()
  return useOptimisticPatch({
    queryKey: settingsQuery.queryKey,
    mutationFn: async (on: boolean) => (await api.PATCH('/api/settings', { body: { [key]: on } })).data!,
    apply: (settings: Settings, on) => ({ ...settings, [key]: on }),
    undo: (_on, before) => before[key],
    onSuccess: (saved) => client.setQueryData(settingsQuery.queryKey, saved),
  })
}
