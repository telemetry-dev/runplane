import { Select as BaseSelect } from '@base-ui/react/select'
import { ChevronDown } from 'lucide-react'
import { useMemo } from 'react'
import { SelectItem, SelectPopup } from '~/components/ui/select'
import { cx } from '~/lib/cx'

type FilterSelectProps<T extends string> = {
  /** Muted name before the value, as "Status" in "Status Any". Without it, pass `aria-label`. */
  label?: string
  /** `null` is no filter, shown as `allLabel`. */
  value: T | null
  onValueChange: (value: T | null) => void
  options: ReadonlyArray<{ value: T; label: string }>
  /** The option for no filter. Omit it for a filter that always has a value, as the time range. */
  allLabel?: string
  'aria-label'?: string
  className?: string
}

/** A compact filter button of the Jobs toolbar, as "Status Any ⌄" and "Last 7 days ⌄". */
export function FilterSelect<T extends string>({ label, value, onValueChange, options, allLabel, className, ...props }: FilterSelectProps<T>) {
  const items = useMemo(
    () => (allLabel === undefined ? options : [{ value: null, label: allLabel }, ...options]),
    [allLabel, options],
  )
  return (
    <BaseSelect.Root items={items} value={value} onValueChange={onValueChange}>
      <BaseSelect.Trigger
        aria-label={props['aria-label'] ?? label}
        className={cx(
          'flex shrink-0 items-center gap-xs rounded-s bg-(--input-background,var(--neutral-1)) px-s py-xs text-s whitespace-nowrap select-none',
          'transition-[background-color] data-disabled:bg-neutral-3 data-disabled:text-neutral-6',
          className,
        )}
      >
        {label && <span className="text-neutral-7">{label}</span>}
        <BaseSelect.Value className="font-medium text-neutral-10" />
        <BaseSelect.Icon className="flex text-neutral-7">
          <ChevronDown size={14} aria-hidden />
        </BaseSelect.Icon>
      </BaseSelect.Trigger>
      <SelectPopup minWidth={180}>
        {items.map((item) => (
          <SelectItem key={item.value ?? ''} value={item.value}>
            {item.label}
          </SelectItem>
        ))}
      </SelectPopup>
    </BaseSelect.Root>
  )
}
