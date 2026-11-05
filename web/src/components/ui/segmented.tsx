import { Toggle } from '@base-ui/react/toggle'
import { ToggleGroup } from '@base-ui/react/toggle-group'
import { Link, type LinkProps } from '@tanstack/react-router'
import type { ReactNode } from 'react'
import { cx } from '~/lib/cx'
import { tabItem, tabList } from './tabs'

// The pill strip of gui/components/runplane/tabs.md for a value or for routes,
// where Tabs (which switch panels) do not fit: the Overview range (24h, 7d, 30d)
// and the Settings sections.

type SegmentedControlProps<T extends string> = {
  value: T
  onValueChange: (value: T) => void
  options: ReadonlyArray<{ value: T; label: ReactNode }>
  'aria-label': string
}

export function SegmentedControl<T extends string>({ value, onValueChange, options, ...props }: SegmentedControlProps<T>) {
  return (
    <ToggleGroup
      value={[value]}
      onValueChange={(values) => {
        // A press on the selected option empties the group; keep the current value.
        const next = values[0] as T | undefined
        if (next) onValueChange(next)
      }}
      className={tabList}
      {...props}
    >
      {options.map((option) => (
        <Toggle key={option.value} value={option.value} className={cx(tabItem, 'data-pressed:bg-neutral-1 data-pressed:text-neutral-10 data-pressed:shadow-s')}>
          {option.label}
        </Toggle>
      ))}
    </ToggleGroup>
  )
}

type SegmentedLinksProps = {
  links: ReadonlyArray<{ to: LinkProps['to']; label: ReactNode }>
  'aria-label': string
}

export function SegmentedLinks({ links, ...props }: SegmentedLinksProps) {
  return (
    <nav className={tabList} {...props}>
      {links.map((link) => (
        <Link
          key={String(link.to)}
          to={link.to}
          className={cx(tabItem, 'aria-[current=page]:bg-neutral-1 aria-[current=page]:text-neutral-10 aria-[current=page]:shadow-s')}
        >
          {link.label}
        </Link>
      ))}
    </nav>
  )
}
