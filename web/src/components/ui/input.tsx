import { Input as BaseInput } from '@base-ui/react/input'
import { Search } from 'lucide-react'
import { cx } from '~/lib/cx'

type InputProps = Omit<BaseInput.Props, 'className'> & { className?: string }

// The theme draws no input edge (border width 0), so the fill must differ from
// the surface: neutral-1 on the neutral-2 cards of the designs. A container on
// neutral-1 (Dialog does this) sets --input-background.
const field = cx(
  'min-w-0 rounded-s bg-(--input-background,var(--neutral-1)) px-s py-xs text-s text-neutral-10',
  'transition-[background-color,color]',
)

/** Text input of gui/components/runplane/input.md. */
export function Input({ className, ...props }: InputProps) {
  return (
    <BaseInput
      className={cx(
        field,
        'w-full placeholder:text-neutral-7',
        'read-only:bg-neutral-2 read-only:text-neutral-8',
        'data-disabled:bg-neutral-3 data-disabled:text-neutral-6',
        className,
      )}
      {...props}
    />
  )
}

type SearchInputProps = Omit<InputProps, 'type'> & {
  /** Classes of the outer field, for example a width. In a flex row it takes its content width unless it grows. */
  className?: string
}

/**
 * Input with a leading search icon, as "Search job, branch or commit" in Jobs and
 * "Find in logs" in Job detail. The outer field takes the focus outline.
 */
export function SearchInput({ className, ...props }: SearchInputProps) {
  return (
    <label
      className={cx(
        field,
        'flex cursor-text items-center gap-xs has-focus-visible:outline-2 has-focus-visible:outline-(--cte-focus)',
        'has-data-disabled:bg-neutral-3 has-data-disabled:text-neutral-6',
        className,
      )}
    >
      <Search size={14} className="text-neutral-7" aria-hidden />
      <BaseInput
        type="search"
        className="w-full min-w-0 bg-transparent outline-none placeholder:text-neutral-7 [&::-webkit-search-cancel-button]:hidden"
        {...props}
      />
    </label>
  )
}
