import { AlertDialog as BaseAlertDialog } from '@base-ui/react/alert-dialog'
import type { CSSProperties, ReactNode } from 'react'
import { cx } from '~/lib/cx'
import { modalBackdrop, modalPopup } from './dialog'

// Confirmation of gui/components/runplane/alert-dialog.md. It has no corner
// close and outside presses do not dismiss it; the footer holds a Cancel close
// and the explicit confirm action:
//
//   <AlertDialog>
//     <AlertDialogTrigger render={<Button />}>Remove</AlertDialogTrigger>
//     <AlertDialogPopup title="Remove fsn1-ax52-b?" description="Running jobs on it are cancelled.">
//       <AlertDialogFooter>
//         <AlertDialogClose render={<Button />}>Cancel</AlertDialogClose>
//         <Button variant="danger" onClick={remove}>Remove server</Button>
//       </AlertDialogFooter>
//     </AlertDialogPopup>
//   </AlertDialog>

export const AlertDialog = BaseAlertDialog.Root
export const AlertDialogTrigger = BaseAlertDialog.Trigger
export const AlertDialogClose = BaseAlertDialog.Close

type AlertDialogPopupProps = Omit<BaseAlertDialog.Popup.Props, 'className' | 'style' | 'title'> & {
  title: ReactNode
  description?: ReactNode
  className?: string
}

export function AlertDialogPopup({ title, description, className, children, ...props }: AlertDialogPopupProps) {
  return (
    <BaseAlertDialog.Portal>
      <BaseAlertDialog.Backdrop className={modalBackdrop} />
      <BaseAlertDialog.Popup className={cx(modalPopup, className)} style={{ '--dialog-width': '400px' } as CSSProperties} {...props}>
        <div className="flex flex-col gap-xs">
          <BaseAlertDialog.Title className="text-m font-medium text-neutral-10">{title}</BaseAlertDialog.Title>
          {description && <BaseAlertDialog.Description className="text-s text-neutral-7">{description}</BaseAlertDialog.Description>}
        </div>
        {children}
      </BaseAlertDialog.Popup>
    </BaseAlertDialog.Portal>
  )
}

export function AlertDialogFooter({ children }: { children: ReactNode }) {
  return <div className="flex flex-wrap justify-end gap-xs">{children}</div>
}
