import { Dialog as BaseDialog } from '@base-ui/react/dialog'
import { X } from 'lucide-react'
import type { CSSProperties, ReactNode } from 'react'
import { cx } from '~/lib/cx'

export const Dialog = BaseDialog.Root
export const DialogTrigger = BaseDialog.Trigger
export const DialogClose = BaseDialog.Close

type DialogPopupProps = Omit<BaseDialog.Popup.Props, 'className' | 'style' | 'title'> & {
  title: ReactNode
  /** Popup width in pixels before the viewport limit. The designs use 400 and 520. */
  width?: number
  className?: string
}

/** Modal of gui/components/runplane/dialog.md and the "Servers · Add server" design: title row with close, then content. */
export function DialogPopup({ title, width = 400, className, children, ...props }: DialogPopupProps) {
  return (
    <BaseDialog.Portal>
      <BaseDialog.Backdrop className={modalBackdrop} />
      <BaseDialog.Popup
        className={cx(modalPopup, className)}
        style={{ '--dialog-width': `${width}px` } as CSSProperties}
        {...props}
      >
        <div className="flex items-center justify-between gap-m">
          <BaseDialog.Title className="text-m font-medium tracking-l">{title}</BaseDialog.Title>
          <BaseDialog.Close
            aria-label="Close"
            className="-m-xxs grid place-items-center rounded-s p-xxs text-neutral-7 transition-colors hover:bg-neutral-3 hover:text-neutral-10"
          >
            <X size={16} aria-hidden />
          </BaseDialog.Close>
        </div>
        {children}
      </BaseDialog.Popup>
    </BaseDialog.Portal>
  )
}

export function DialogDescription({ children }: { children: ReactNode }) {
  return <BaseDialog.Description className="text-s text-neutral-8">{children}</BaseDialog.Description>
}

export function DialogFooter({ children }: { children: ReactNode }) {
  return <div className="flex flex-wrap justify-end gap-xs">{children}</div>
}

// Shared with AlertDialog: a centered popup over a backdrop, both with Large motion.
export const modalBackdrop = cx(
  'fixed inset-zero z-20 bg-neutral-10-transparent dark:bg-neutral-1-transparent',
  'transition-opacity duration-(--motion-large-duration) ease-large',
  'data-starting-style:opacity-0 data-ending-style:opacity-0',
)

export const modalPopup = cx(
  'fixed top-1/2 left-1/2 z-20 flex -translate-1/2 flex-col gap-l rounded-m p-l',
  'max-h-[calc(100dvh-var(--space-xl))] w-[min(var(--dialog-width),calc(100vw-var(--space-xl)))] overflow-y-auto',
  'bg-neutral-1 text-neutral-10 shadow-l dark:bg-neutral-2',
  // Inputs and code blocks need a fill that differs from the white popup.
  '[--code-background:var(--neutral-3)] [--code-highlight:var(--neutral-4)] [--input-background:var(--neutral-3)]',
  'transition-[opacity,scale] duration-(--motion-large-duration) ease-large',
  'data-starting-style:scale-(--motion-popup-scale) data-starting-style:opacity-0',
  'data-ending-style:scale-(--motion-popup-scale) data-ending-style:opacity-0',
)
