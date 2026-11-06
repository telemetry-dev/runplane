import { useQueryClient } from '@tanstack/react-query'
import { $api, api, errorMessage, isApiError, type Schemas } from '~/api/client'
import { applyChange } from '~/api/events'
import { useOptimisticPatch } from '~/api/optimistic-patch'
import { settingsQuery } from '~/api/settings'
import { toastManager } from '~/components/ui/toast'

type Settings = Schemas['Settings']
type SettingsPatch = Schemas['SettingsPatch']

/** The settings keys that a Switch changes: the settings that are on or off. */
export type SwitchKey = { [K in keyof SettingsPatch]-?: NonNullable<SettingsPatch[K]> extends boolean ? K : never }[keyof SettingsPatch]

/**
 * PATCH /api/settings for one control, so each control has its own pending
 * state and error. The response changes the cache as its settings.changed
 * event does.
 */
export function useSaveSettings() {
  const client = useQueryClient()
  return $api.useMutation('patch', '/api/settings', {
    onSuccess: (saved) => void applyChange(client, 'settings.changed', saved),
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
    onSuccess: (saved) => void applyChange(client, 'settings.changed', saved),
  })
}

/**
 * The message for a failed save of one control. Its body holds only its own
 * keys, so every field problem of the response belongs to it.
 */
export function saveError(error: unknown, line?: (index: number, message: string) => string): string | undefined {
  if (!error) return undefined
  if (!isApiError(error) || !error.errors?.length) return errorMessage(error)
  const messages = error.errors.map(({ location = '', message = '' }) => {
    const index = /\[(\d+)\]$/.exec(location)?.[1]
    return line && index !== undefined ? line(Number(index), message) : message
  })
  return [...new Set(messages)].join(' ')
}

let savedToast: string | undefined

/** One quiet "Saved" toast; a new save replaces the last one instead of stacking. */
export function showSaved() {
  if (savedToast) toastManager.close(savedToast)
  savedToast = toastManager.add({ title: 'Saved' })
}
