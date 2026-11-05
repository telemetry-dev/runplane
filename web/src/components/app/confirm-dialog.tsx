import { useMutation } from '@tanstack/react-query'
import type { ReactNode } from 'react'
import { errorMessage } from '~/api/client'
import { AlertDialog, AlertDialogClose, AlertDialogFooter, AlertDialogPopup } from '~/components/ui/alert-dialog'
import { Button } from '~/components/ui/button'

type ConfirmDialogProps = {
  open: boolean
  onOpenChange: (open: boolean) => void
  title: ReactNode
  /** What the change does, as "Running jobs on it are cancelled." */
  description: ReactNode
  /** The label of the button that makes the change, as "Remove server". */
  action: string
  /** `primary` for a change that is easy to undo, as Drain. */
  variant?: 'danger' | 'primary'
  /** The change. The dialog closes when it succeeds and shows its error when it does not. */
  onConfirm: () => Promise<unknown>
  /**
   * Runs when the dialog of a successful change has closed. A dialog in a table
   * row removes its row here, so the row stays for the close animation.
   */
  onDone?: () => void
}

/** Asks before a change, as a removal: the title, what the change does, Cancel, and the action. */
export function ConfirmDialog({ open, onOpenChange, title, description, action, variant = 'danger', onConfirm, onDone }: ConfirmDialogProps) {
  const confirm = useMutation({ mutationFn: onConfirm, onSuccess: () => onOpenChange(false) })

  return (
    <AlertDialog
      open={open}
      onOpenChange={onOpenChange}
      onOpenChangeComplete={(isOpen) => {
        if (isOpen) return
        if (confirm.isSuccess) onDone?.()
        confirm.reset()
      }}
    >
      <AlertDialogPopup title={title} description={description}>
        {confirm.error && (
          <p role="alert" className="text-s text-error">
            {errorMessage(confirm.error)}
          </p>
        )}
        <AlertDialogFooter>
          <AlertDialogClose render={<Button />}>Cancel</AlertDialogClose>
          <Button variant={variant} loading={confirm.isPending} onClick={() => confirm.mutate()}>
            {action}
          </Button>
        </AlertDialogFooter>
      </AlertDialogPopup>
    </AlertDialog>
  )
}
