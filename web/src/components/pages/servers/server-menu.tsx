import { useQueryClient } from '@tanstack/react-query'
import { Link } from '@tanstack/react-router'
import { ArrowRight, Ellipsis, Pause, Play, Stethoscope, Trash2 } from 'lucide-react'
import { useState } from 'react'
import { $api, api, errorMessage } from '~/api/client'
import { applyChange } from '~/api/events'
import { ConfirmDialog } from '~/components/app/confirm-dialog'
import { IconButton } from '~/components/ui/button'
import { Menu, MenuItem, MenuLinkItem, MenuPopup, MenuSeparator, MenuTrigger } from '~/components/ui/menu'
import { toastManager } from '~/components/ui/toast'
import { DoctorDialog } from './doctor-dialog'
import { removeDescription, type Server } from './server-info'
import { UpdateAgentItem } from './update-agent-item'

/** The ⋯ menu of a server row: Open, Drain or Resume, Run doctor, Update agent, Remove. For admins only. */
export function ServerMenu({ server }: { server: Server }) {
  const client = useQueryClient()
  const [dialog, setDialog] = useState<'doctor' | 'remove'>()
  const { actions } = server

  const update = $api.useMutation('patch', '/api/servers/{id}', {
    onSuccess: (updated) => applyChange(client, 'server.state', updated),
    onError: (error, { body }) =>
      toastManager.add({ title: `Could not ${body.status === 'online' ? 'resume' : 'drain'} ${server.name}`, description: errorMessage(error) }),
  })

  function setStatus(status: 'online' | 'draining') {
    update.mutate({ params: { path: { id: server.id } }, body: { status } })
  }

  return (
    <>
      <Menu>
        <MenuTrigger render={<IconButton label={`${server.name} actions`} className="-m-xs" />}>
          <Ellipsis aria-hidden />
        </MenuTrigger>
        <MenuPopup align="end">
          <MenuLinkItem render={<Link to="/servers/$serverId" params={{ serverId: server.id }} />} icon={<ArrowRight />}>
            Open
          </MenuLinkItem>
          {actions.can_resume ? (
            <MenuItem icon={<Play />} onClick={() => setStatus('online')}>
              Resume
            </MenuItem>
          ) : (
            <MenuItem icon={<Pause />} disabled={!actions.can_drain} onClick={() => setStatus('draining')}>
              Drain
            </MenuItem>
          )}
          <MenuItem icon={<Stethoscope />} disabled={!actions.can_run_doctor} onClick={() => setDialog('doctor')}>
            Run doctor
          </MenuItem>
          <UpdateAgentItem server={server} />
          <MenuSeparator />
          <MenuItem icon={<Trash2 />} danger disabled={!actions.can_remove} onClick={() => setDialog('remove')}>
            Remove
          </MenuItem>
        </MenuPopup>
      </Menu>
      <DoctorDialog server={server} open={dialog === 'doctor'} onOpenChange={(open) => setDialog(open ? 'doctor' : undefined)} />
      <RemoveDialog server={server} open={dialog === 'remove'} onOpenChange={(open) => setDialog(open ? 'remove' : undefined)} />
    </>
  )
}

type RemoveDialogProps = { server: Server; open: boolean; onOpenChange: (open: boolean) => void }

function RemoveDialog({ server, open, onOpenChange }: RemoveDialogProps) {
  const client = useQueryClient()
  return (
    <ConfirmDialog
      open={open}
      onOpenChange={onOpenChange}
      title={`Remove ${server.name}?`}
      description={removeDescription(server.running_jobs)}
      action="Remove server"
      onConfirm={() => api.DELETE('/api/servers/{id}', { params: { path: { id: server.id } } })}
      onDone={() => void applyChange(client, 'server.removed', { id: server.id })}
    />
  )
}
