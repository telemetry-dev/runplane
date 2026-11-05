import { Drawer as BaseDrawer } from '@base-ui/react/drawer'
import { X } from 'lucide-react'
import type { CSSProperties, ReactNode } from 'react'
import { cx } from '~/lib/cx'
import { IconButton } from './button'

// Drawer of gui/components/runplane/drawer.md: a bottom sheet that the user can
// swipe down to close. `<Drawer swipeDirection="right">` makes a side panel at the
// right edge instead; the popup follows the root's swipe direction.
//
//   <Drawer>
//     <DrawerTrigger render={<Button />}>Details</DrawerTrigger>
//     <DrawerPopup title="runplane-8cpu">…</DrawerPopup>
//   </Drawer>

export const Drawer = BaseDrawer.Root
export const DrawerTrigger = BaseDrawer.Trigger
export const DrawerClose = BaseDrawer.Close

type DrawerPopupProps = Omit<BaseDrawer.Popup.Props, 'className' | 'style' | 'title' | 'children'> & {
  title: ReactNode
  description?: ReactNode
  /** Popup width in pixels before the viewport limit, for example wider for a log. */
  width?: number
  className?: string
  children?: ReactNode
}

export function DrawerPopup({ title, description, width = 420, className, children, ...props }: DrawerPopupProps) {
  return (
    <BaseDrawer.Portal>
      <BaseDrawer.Backdrop
        className={cx(
          'fixed inset-zero z-20 bg-neutral-10-transparent dark:bg-neutral-1-transparent',
          'opacity-[calc(1-var(--drawer-swipe-progress,0))] transition-opacity duration-(--motion-large-duration) ease-large',
          'data-starting-style:opacity-0 data-ending-style:opacity-0 data-swiping:duration-0',
        )}
      />
      <BaseDrawer.Viewport className="fixed inset-zero z-20 flex items-end justify-center has-data-[swipe-direction=right]:items-stretch has-data-[swipe-direction=right]:justify-end">
        <BaseDrawer.Popup
          className={cx(
            'group flex max-h-[85vh] w-[min(var(--drawer-width),calc(100vw-var(--space-xl)))] flex-col overflow-y-auto overscroll-contain rounded-t-m p-l outline-none',
            'bg-neutral-1 text-neutral-10 shadow-l dark:bg-neutral-2',
            '[--code-background:var(--neutral-3)] [--code-highlight:var(--neutral-4)] [--input-background:var(--neutral-3)]',
            'transition-transform duration-(--motion-large-duration) ease-large data-swiping:duration-0 data-swiping:select-none',
            '[transform:translateY(var(--drawer-swipe-movement-y))]',
            'data-starting-style:[transform:translateY(100%)] data-ending-style:[transform:translateY(100%)]',
            // Side panel at the right edge.
            'data-[swipe-direction=right]:max-h-none data-[swipe-direction=right]:rounded-t-zero data-[swipe-direction=right]:rounded-l-m',
            'data-[swipe-direction=right]:[transform:translateX(var(--drawer-swipe-movement-x))]',
            'data-[swipe-direction=right]:data-starting-style:[transform:translateX(100%)]',
            'data-[swipe-direction=right]:data-ending-style:[transform:translateX(100%)]',
            className,
          )}
          style={{ '--drawer-width': `${width}px` } as CSSProperties}
          {...props}
        >
          <div
            aria-hidden
            className="mx-auto mb-xs h-xxs w-[calc(2*var(--space-xl))] shrink-0 rounded-full bg-neutral-4 group-data-[swipe-direction=right]:hidden"
          />
          <BaseDrawer.Content className="flex flex-col gap-s">
            <div className="flex items-start justify-between gap-m">
              <div className="flex min-w-0 flex-col gap-xxs">
                <BaseDrawer.Title className="text-m font-medium text-neutral-10">{title}</BaseDrawer.Title>
                {description && <BaseDrawer.Description className="text-s text-neutral-7">{description}</BaseDrawer.Description>}
              </div>
              <BaseDrawer.Close render={<IconButton label="Close" className="-m-xxs p-xxs" />}>
                <X aria-hidden />
              </BaseDrawer.Close>
            </div>
            {children}
          </BaseDrawer.Content>
        </BaseDrawer.Popup>
      </BaseDrawer.Viewport>
    </BaseDrawer.Portal>
  )
}

export function DrawerFooter({ children }: { children: ReactNode }) {
  return <div className="mt-xs flex flex-wrap justify-end gap-xs">{children}</div>
}
