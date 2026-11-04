import { useQueryClient } from '@tanstack/react-query'
import { CircleArrowUp } from 'lucide-react'
import { $api, errorMessage } from '~/api/client'
import { putServer } from '~/components/pages/server-detail/queries'
import { MenuItem } from '~/components/ui/menu'
import { toastManager } from '~/components/ui/toast'
import type { Server } from './server-info'

/**
 * The "Update agent" item of a server menu, for an agent that does not run the
 * release of the primary. POST /api/servers/{id}/update starts the update, and
 * server.state events show it. The mutation callbacks run also after the menu
 * closes and this item unmounts.
 */
export function UpdateAgentItem({ server }: { server: Server }) {
  const client = useQueryClient()
  const update = $api.useMutation('post', '/api/servers/{id}/update', {
    onSuccess: (updated) => {
      void putServer(client, updated)
      toastManager.add({ title: `Updating ${server.name}`, description: 'The agent restarts. Its runners keep running.' })
    },
    onError: (error) => toastManager.add({ title: `Could not update ${server.name}`, description: errorMessage(error) }),
  })

  const state = server.update_state
  if (state !== 'available' && state !== 'failed' && state !== 'updating') return null
  return (
    <MenuItem
      icon={<CircleArrowUp />}
      disabled={state === 'updating' || update.isPending}
      onClick={() => update.mutate({ params: { path: { id: server.id } } })}
    >
      {state === 'updating' ? 'Updating agent…' : 'Update agent'}
    </MenuItem>
  )
}
