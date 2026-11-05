import { errorMessage, isApiError } from '~/api/client'
import { toastManager } from '~/components/ui/toast'

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
