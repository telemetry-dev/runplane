import { Button } from '@base-ui/react/button'
import { createLink } from '@tanstack/react-router'
import { ChevronRight } from 'lucide-react'
import type { AnchorHTMLAttributes, ReactNode, Ref } from 'react'
import { cx } from '~/lib/cx'

type SectionHeaderProps = {
  title: ReactNode
  /** Muted count after the title, as "Running 7". */
  count?: number
  /** At the right end: a SectionLink, a SectionButton, or a Button. */
  action?: ReactNode
  /** Heading level of the title; the look stays the same. */
  level?: 2 | 3
}

/** Title row of a page section, as "Running 7" and "Recent jobs … All jobs ›" in Overview. */
export function SectionHeader({ title, count, action, level = 2 }: SectionHeaderProps) {
  const Heading = level === 2 ? 'h2' : 'h3'
  return (
    <div className="flex min-h-[22px] items-center justify-between gap-m">
      <Heading className="flex min-w-0 items-baseline gap-xs text-m font-heavy tracking-l text-neutral-10">
        <span className="truncate">{title}</span>
        {count !== undefined && <span className="font-regular text-neutral-6">{count}</span>}
      </Heading>
      {action}
    </div>
  )
}

const textAction = cx(
  'inline-flex shrink-0 items-center gap-xxs rounded-xs text-s font-medium text-neutral-7 transition-colors hover:text-neutral-10',
  'data-disabled:text-neutral-5',
)

type SectionAnchorProps = AnchorHTMLAttributes<HTMLAnchorElement> & {
  ref?: Ref<HTMLAnchorElement>
  /** Adds the trailing chevron of "All jobs ›". */
  chevron?: boolean
}

function SectionAnchor({ chevron = false, className, children, ...props }: SectionAnchorProps) {
  return (
    <a className={cx(textAction, className)} {...props}>
      {children}
      {chevron && <ChevronRight size={14} aria-hidden />}
    </a>
  )
}

/** A quiet router link at the end of a SectionHeader, as "All jobs ›" or "Manage". */
export const SectionLink = createLink(SectionAnchor)

type SectionButtonProps = Omit<Button.Props, 'className'> & { className?: string }

/** A quiet button at the end of a SectionHeader, as "Add server" that opens a dialog. */
export function SectionButton({ className, ...props }: SectionButtonProps) {
  return <Button className={cx(textAction, className)} {...props} />
}
