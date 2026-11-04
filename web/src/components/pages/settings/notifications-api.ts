import { useMutation, useQueryClient } from '@tanstack/react-query'
import { $api, api, errorMessage, type Schemas } from '~/api/client'
import { toastManager } from '~/components/ui/toast'

/** GET /api/notifications, for admins. */
export const notificationsQuery = $api.queryOptions('get', '/api/notifications')

/** A test message to a saved channel, or to the kind and URL of a channel that is not saved yet. */
export type TestTarget = { name: string } & ({ id: string } | Schemas['TestChannelRequest'])

/**
 * Sends a test message and shows a toast for the result. A test of a saved
 * channel also records its last delivery, so the list refetches.
 */
export function useSendTest() {
  const client = useQueryClient()
  return useMutation({
    mutationFn: (target: TestTarget) =>
      'id' in target
        ? api.POST('/api/notifications/{id}/test', { params: { path: { id: target.id } } })
        : api.POST('/api/notifications/test', { body: { kind: target.kind, url: target.url } }),
    onSuccess: (_data, target) => toastManager.add({ title: `Test message sent to ${target.name}` }),
    onError: (error, target) => toastManager.add({ title: `Could not send a test to ${target.name}`, description: errorMessage(error) }),
    onSettled: (_data, _error, target) => {
      if ('id' in target) void client.invalidateQueries({ queryKey: notificationsQuery.queryKey })
    },
  })
}
