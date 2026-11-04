import { Toggle as BaseToggle } from '@base-ui/react/toggle'
import { ToggleGroup as BaseToggleGroup } from '@base-ui/react/toggle-group'
import { cx } from '~/lib/cx'

// Toggle and ToggleGroup of gui/components/runplane/toggle.md and toggle-group.md:
// content-sized buttons that stay pressed. For a single choice from a few values
// on a pill strip (24h, 7d, 30d) use SegmentedControl from ./segmented.

type ToggleProps = Omit<BaseToggle.Props, 'className'> & { className?: string }

export function Toggle({ className, ...props }: ToggleProps) {
  return (
    <BaseToggle
      className={cx(
        'inline-flex items-center justify-center gap-xs rounded-s px-s py-xs text-s font-medium whitespace-nowrap text-neutral-10 select-none',
        'transition-[background-color,color] not-data-disabled:hover:bg-neutral-3 data-pressed:bg-neutral-3',
        'data-disabled:cursor-not-allowed data-disabled:text-neutral-6 [&_svg]:size-[14px]',
        className,
      )}
      {...props}
    />
  )
}

type ToggleGroupProps = Omit<BaseToggleGroup.Props, 'className'> & { className?: string }

/** A row of Toggles with roving focus. Set `multiple` for independent toggles; without it one stays pressed. */
export function ToggleGroup({ className, ...props }: ToggleGroupProps) {
  return <BaseToggleGroup className={cx('flex items-center gap-xxs', className)} {...props} />
}
