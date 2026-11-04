import { Select as BaseSelect } from '@base-ui/react/select'
import { Check, ChevronDown } from 'lucide-react'
import type { CSSProperties, ReactNode } from 'react'
import { cx } from '~/lib/cx'
import { option, popupMotion, popupSurface } from './popup'

// Select of gui/components/runplane/select.md. Compose the parts:
//
//   <Select items={pools} value={poolId} onValueChange={setPoolId}>
//     <SelectLabel>Pool</SelectLabel>
//     <SelectTrigger placeholder="Choose a pool" />
//     <SelectPopup>
//       {pools.map((pool) => <SelectItem key={pool.value} value={pool.value}>{pool.label}</SelectItem>)}
//     </SelectPopup>
//   </Select>
//
// Pass `items` ({ value, label }[]) so the trigger shows the label of the value
// before the popup first opens.

export const Select = BaseSelect.Root
export const SelectGroup = BaseSelect.Group

export function SelectLabel({ children }: { children: ReactNode }) {
  return (
    <BaseSelect.Label className="cursor-default text-s font-medium text-neutral-10 data-disabled:text-neutral-6">
      {children}
    </BaseSelect.Label>
  )
}

type SelectTriggerProps = Omit<BaseSelect.Trigger.Props, 'className' | 'children'> & {
  placeholder?: ReactNode
  className?: string
}

export function SelectTrigger({ placeholder, className, ...props }: SelectTriggerProps) {
  return (
    <BaseSelect.Trigger
      className={cx(
        'flex w-full min-w-0 items-center justify-between gap-xs rounded-s px-s py-xs text-left select-none',
        'bg-(--input-background,var(--neutral-1)) text-s text-neutral-10 transition-[background-color,color]',
        'data-disabled:cursor-not-allowed data-disabled:bg-neutral-3 data-disabled:text-neutral-6',
        className,
      )}
      {...props}
    >
      <BaseSelect.Value placeholder={placeholder} className="truncate data-placeholder:text-neutral-7" />
      <BaseSelect.Icon className="flex text-neutral-7">
        <ChevronDown size="1em" aria-hidden />
      </BaseSelect.Icon>
    </BaseSelect.Trigger>
  )
}

type SelectPopupProps = {
  /** Minimum popup width in pixels, for triggers narrower than their options. */
  minWidth?: number
  align?: BaseSelect.Positioner.Props['align']
  children: ReactNode
}

/** Opens below the trigger, as wide as the trigger (or `minWidth`), and scrolls when it is taller than the space. */
export function SelectPopup({ minWidth = 0, align = 'start', children }: SelectPopupProps) {
  return (
    <BaseSelect.Portal>
      <BaseSelect.Positioner alignItemWithTrigger={false} side="bottom" align={align} sideOffset={3} className="z-20 outline-none">
        <BaseSelect.Popup
          className={cx(
            'rounded-s p-xxs',
            'w-[max(var(--anchor-width),var(--select-min-width))] max-w-(--available-width)',
            popupSurface,
            popupMotion,
          )}
          style={{ '--select-min-width': `${minWidth}px` } as CSSProperties}
        >
          <BaseSelect.List className="flex max-h-[min(360px,var(--available-height))] flex-col gap-xxs overflow-y-auto overscroll-contain outline-none">
            {children}
          </BaseSelect.List>
        </BaseSelect.Popup>
      </BaseSelect.Positioner>
    </BaseSelect.Portal>
  )
}

type SelectItemProps = Omit<BaseSelect.Item.Props, 'className' | 'children'> & { children: ReactNode }

export function SelectItem({ children, ...props }: SelectItemProps) {
  return (
    <BaseSelect.Item
      className={cx(option, 'flex items-center justify-between gap-xs data-selected:not-data-highlighted:bg-neutral-2')}
      {...props}
    >
      <BaseSelect.ItemText className="truncate">{children}</BaseSelect.ItemText>
      <BaseSelect.ItemIndicator className="flex shrink-0">
        <Check size="1em" aria-hidden />
      </BaseSelect.ItemIndicator>
    </BaseSelect.Item>
  )
}

export function SelectGroupLabel({ children }: { children: ReactNode }) {
  return <BaseSelect.GroupLabel className="px-xs pt-xs pb-xxs text-xs font-medium text-neutral-6">{children}</BaseSelect.GroupLabel>
}

export function SelectSeparator() {
  return <BaseSelect.Separator className="my-xxs h-px shrink-0 shadow-[inset_0_1px_0_var(--neutral-4)]" />
}
