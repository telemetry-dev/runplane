import { useQuery, useQueryClient } from '@tanstack/react-query'
import { useMemo } from 'react'
import { $api, errorMessage, fieldErrors, isApiError, type Schemas } from '~/api/client'
import { sessionQuery, statusQuery } from '~/api/queries'
import { toastManager } from '~/components/ui/toast'

export type Settings = Schemas['Settings']
export type SettingsPatch = Schemas['SettingsPatch']

/** GET /api/settings. Every user can read it; only admins can change it. */
export const settingsQuery = $api.queryOptions('get', '/api/settings')

export function useIsAdmin(): boolean {
  return useQuery(sessionQuery).data?.role === 'admin'
}

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
    },
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

/**
 * The field messages of a failed form submit for a Base UI Form, and the
 * message of an error without fields. The Form puts its `errors` back whenever
 * the object changes, so it stays the same object until the error changes;
 * then a field's message goes away when the user edits that field. `prefix`
 * comes off the field names, as "backup_s3." for the S3 fields.
 */
export function useFormErrors(error: unknown, prefix = '') {
  return useMemo(() => {
    const fields = Object.fromEntries(
      Object.entries(fieldErrors(error)).map(([location, message]) => [
        location.slice(location.startsWith(prefix) ? prefix.length : 0),
        message,
      ]),
    )
    const other = error && Object.keys(fields).length === 0 ? errorMessage(error) : undefined
    return { fields, other }
  }, [error, prefix])
}
