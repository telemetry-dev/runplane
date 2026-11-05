import { Popover as BasePopover } from '@base-ui/react/popover'
import type { ReactNode } from 'react'
import { cx } from '~/lib/cx'
import { popupMotion, popupSurface } from './popup'

// Popover of gui/components/runplane/popover.md:
//
//   <Popover>
//     <PopoverTrigger render={<Button />}>Details</PopoverTrigger>
//     <PopoverPopup title="Reject reason">…</PopoverPopup>
//   </Popover>

export const Popover = BasePopover.Root
export const PopoverTrigger = BasePopover.Trigger

type PopoverPopupProps = {
  title?: ReactNode
  description?: ReactNode
  className?: string
  children?: ReactNode
}

/** A 300px panel, narrower when the space is smaller; it scrolls when taller than the space. */
export function PopoverPopup({ title, description, className, children }: PopoverPopupProps) {
  return (
    <BasePopover.Portal>
      <BasePopover.Positioner side="bottom" align="center" sideOffset={6} collisionPadding={12} className="z-20 outline-none">
        <BasePopover.Popup
          className={cx(
            'flex w-[min(300px,var(--available-width),calc(100vw-var(--space-xl)))] flex-col gap-xs rounded-m p-m',
            'max-h-(--available-height) overflow-y-auto overscroll-contain',
            '[--code-background:var(--neutral-2)] [--input-background:var(--neutral-3)]',
            popupSurface,
            popupMotion,
            className,
          )}
        >
          {title && <BasePopover.Title className="text-m font-medium text-neutral-10">{title}</BasePopover.Title>}
          {description && <BasePopover.Description className="max-w-[40ch] text-s text-neutral-7">{description}</BasePopover.Description>}
          {children}
        </BasePopover.Popup>
      </BasePopover.Positioner>
    </BasePopover.Portal>
  )
}
