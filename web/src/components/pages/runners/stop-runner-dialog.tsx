import { useQueryClient } from '@tanstack/react-query'
import { $api, errorMessage } from '~/api/client'
import { applyChange } from '~/api/events'
import { AlertDialog, AlertDialogClose, AlertDialogFooter, AlertDialogPopup } from '~/components/ui/alert-dialog'
import { Button } from '~/components/ui/button'
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
  const stop = $api.useMutation('post', '/api/sandboxes/{id}/stop', {
    onSuccess: () => {
      onOpenChange(false)
      toastManager.add({ title: `Stopping ${sandbox.runner_name}` })
      void applyChange(client, 'sandbox.state', sandbox)
    },
  })

  return (
    <AlertDialog open={open} onOpenChange={onOpenChange} onOpenChangeComplete={(isOpen) => !isOpen && stop.reset()}>
      <AlertDialogPopup title={`Stop ${sandbox.runner_name}?`} description={stopDescription(sandbox)}>
        {stop.error && (
          <p role="alert" className="text-s text-error">
            {errorMessage(stop.error)}
          </p>
        )}
        <AlertDialogFooter>
          <AlertDialogClose render={<Button />}>Cancel</AlertDialogClose>
          <Button variant="danger" loading={stop.isPending} onClick={() => stop.mutate({ params: { path: { id: sandbox.id } } })}>
            Stop runner
          </Button>
        </AlertDialogFooter>
      </AlertDialogPopup>
    </AlertDialog>
  )
}

function stopDescription(sandbox: Sandbox): string {
  if (sandbox.job_id && sandbox.state === 'busy') return `Its job ${sandbox.job_name} stops and fails.`
  return 'A new runner starts when the label needs one.'
}
