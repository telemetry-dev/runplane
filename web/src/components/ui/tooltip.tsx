import { Tooltip as BaseTooltip } from '@base-ui/react/tooltip'
import type { ReactElement, ReactNode } from 'react'
import { cx } from '~/lib/cx'
import { popupMotion } from './popup'

type TooltipProps = Omit<BaseTooltip.Root.Props, 'children'> & {
  /** Short text. It adds to the trigger's own accessible name; it never replaces it. */
  content: ReactNode
  /** The trigger element. It receives the hover and focus handlers. */
  children: ReactElement
}

/** Tooltip of gui/components/runplane/tooltip.md: neutral-10 label with neutral-1 text. */
export function Tooltip({ content, children, ...props }: TooltipProps) {
  return (
    <BaseTooltip.Root {...props}>
      <BaseTooltip.Trigger delay={400} render={children} />
      <BaseTooltip.Portal>
        <BaseTooltip.Positioner side="top" align="center" sideOffset={6} collisionPadding={12} className="z-30">
          <BaseTooltip.Popup
            className={cx(
              'max-w-[min(32ch,var(--available-width))] rounded-s bg-neutral-10 px-xs py-xxs break-words shadow-s',
              'text-xs font-medium text-neutral-1',
              popupMotion,
            )}
          >
            {content}
          </BaseTooltip.Popup>
        </BaseTooltip.Positioner>
      </BaseTooltip.Portal>
    </BaseTooltip.Root>
  )
}
