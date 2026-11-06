import { useQueryClient } from '@tanstack/react-query'
import { api } from '~/api/client'
import { applyChange } from '~/api/events'
import { ConfirmDialog } from '~/components/app/confirm-dialog'
import { toastManager } from '~/components/ui/toast'
import type { Sandbox } from './runner-format'

type StopRunnerDialogProps = {
  sandbox: Sandbox
  open: boolean
  onOpenChange: (open: boolean) => void
}

/** Confirms the stop of a runner. */
export function StopRunnerDialog({ sandbox, open, onOpenChange }: StopRunnerDialogProps) {
  const client = useQueryClient()
  return (
    <ConfirmDialog
      open={open}
      onOpenChange={onOpenChange}
      title={`Stop ${sandbox.runner_name}?`}
      description={stopDescription(sandbox)}
      action="Stop runner"
      onConfirm={async () => {
        await api.POST('/api/sandboxes/{id}/stop', { params: { path: { id: sandbox.id } } })
        toastManager.add({ title: `Stopping ${sandbox.runner_name}` })
        void applyChange(client, 'sandbox.state', sandbox)
      }}
    />
  )
}

function stopDescription(sandbox: Sandbox): string {
  if (sandbox.job_id && sandbox.state === 'busy') return `Its job ${sandbox.job_name} stops and fails.`
  return 'A new runner starts when the label needs one.'
}
