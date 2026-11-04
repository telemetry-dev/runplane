import { Combobox as BaseCombobox } from '@base-ui/react/combobox'
import { Check, ChevronDown } from 'lucide-react'
import type { ReactNode } from 'react'
import { cx } from '~/lib/cx'
import { option, popupMotion, popupSurface } from './popup'

// Combobox of gui/components/runplane/combobox.md, a Select whose options filter
// as the user types. Compose the parts:
//
//   <Combobox items={repositories} value={repository} onValueChange={setRepository}>
//     <ComboboxInput aria-label="Repository" placeholder="Any repository" />
//     <ComboboxPopup empty="No repositories match.">
//       {(name: string) => <ComboboxItem key={name} value={name}>{name}</ComboboxItem>}
//     </ComboboxPopup>
//   </Combobox>
//
// For a visible label, wrap it in a Field: `<Field label="Repository">…</Field>`.

export const Combobox = BaseCombobox.Root

type ComboboxInputProps = Omit<BaseCombobox.Input.Props, 'className'> & { className?: string }

/** The input field with its chevron trigger. Focus on the input outlines the whole field. */
export function ComboboxInput({ className, ...props }: ComboboxInputProps) {
  return (
    <BaseCombobox.InputGroup
      className={cx(
        'flex w-full min-w-0 items-center gap-xs rounded-s bg-(--input-background,var(--neutral-1)) py-xs pr-xxs pl-s',
        'text-s text-neutral-10 transition-[background-color,color]',
        'has-[input:focus-visible]:outline-2 has-[input:focus-visible]:outline-(--cte-focus)',
        'data-disabled:bg-neutral-3 data-disabled:text-neutral-6',
        className,
      )}
    >
      <BaseCombobox.Input
        className="w-0 min-w-0 flex-1 bg-transparent outline-none placeholder:text-current placeholder:opacity-45"
        {...props}
      />
      <BaseCombobox.Trigger
        aria-label="Show options"
        className="grid size-[1.5em] shrink-0 place-items-center rounded-xs text-neutral-7 hover:text-neutral-10"
      >
        <ChevronDown size="1em" aria-hidden />
      </BaseCombobox.Trigger>
    </BaseCombobox.InputGroup>
  )
}

type ComboboxPopupProps = {
  /** Shown when no option matches the input. */
  empty?: ReactNode
  /** Options, or a function that renders each item of the root's `items`. */
  children: BaseCombobox.List.Props['children']
}

/** Anchored to the whole field and as wide as it; scrolls past 360px. */
export function ComboboxPopup({ empty, children }: ComboboxPopupProps) {
  return (
    <BaseCombobox.Portal>
      <BaseCombobox.Positioner side="bottom" align="start" sideOffset={3} className="z-20 outline-none">
        <BaseCombobox.Popup
          className={cx('w-(--anchor-width) max-w-(--available-width) rounded-s p-xxs', popupSurface, popupMotion)}
        >
          {empty && <BaseCombobox.Empty className="text-s text-neutral-7 empty:hidden not-empty:p-s">{empty}</BaseCombobox.Empty>}
          <BaseCombobox.List className="flex max-h-[min(360px,var(--available-height))] flex-col gap-xxs overflow-y-auto overscroll-contain outline-none data-empty:hidden">
            {children}
          </BaseCombobox.List>
        </BaseCombobox.Popup>
      </BaseCombobox.Positioner>
    </BaseCombobox.Portal>
  )
}

type ComboboxItemProps = Omit<BaseCombobox.Item.Props, 'className' | 'children'> & { children: ReactNode }

export function ComboboxItem({ children, ...props }: ComboboxItemProps) {
  return (
    <BaseCombobox.Item
      className={cx(option, 'flex items-center justify-between gap-xs data-selected:not-data-highlighted:bg-neutral-2')}
      {...props}
    >
      <span className="truncate">{children}</span>
      <BaseCombobox.ItemIndicator className="ml-xs flex shrink-0">
        <Check size="1em" aria-hidden />
      </BaseCombobox.ItemIndicator>
    </BaseCombobox.Item>
  )
}
