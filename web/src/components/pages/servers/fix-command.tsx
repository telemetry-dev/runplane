import { CopyButton } from '~/components/app/copy-button'
import { cx } from '~/lib/cx'

/** The shell command that fixes a doctor check, in the mono font with a copy button. */
export function FixCommand({ command, className }: { command: string; className?: string }) {
  return (
    <div className={cx('flex items-start gap-s rounded-s bg-(--code-background,var(--neutral-2)) py-xs pr-xs pl-s', className)}>
      <code className="min-w-0 flex-1 font-code text-xs leading-s tracking-m break-words text-neutral-10">{command}</code>
      {/* As tall as the code line, so the block keeps its height. */}
      <CopyButton text={command} className="-my-xxs p-xxs text-neutral-8 [&_svg]:size-[14px]" />
    </div>
  )
}
