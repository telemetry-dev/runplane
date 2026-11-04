import { cx } from '~/lib/cx'
import { CopyButton } from './copy-button'

/** A line of code. `added` and `removed` lines get a + or − marker and a highlighted row. */
export type CodeLine = string | { text: string; change?: 'added' | 'removed'; highlight?: boolean }

type CodeBlockProps = {
  /** The code, as one string or as lines. */
  code: string | readonly CodeLine[]
  /** File name above the code, as ".github/workflows/ci.yml". */
  title?: string
  lineNumbers?: boolean
  /** Shows a copy button. It copies the code without removed lines. */
  copyable?: boolean
  /** Wraps long lines instead of scrolling them, so a long value, such as a key, shows whole. */
  wrap?: boolean
  className?: string
}

/**
 * Code in the mono font on a neutral-2 panel (neutral-3 inside dialogs), as the
 * workflow diff of the empty Overview and the install command of "Add server".
 * Long lines scroll sideways, or wrap with `wrap`.
 */
export function CodeBlock({ code, title, lineNumbers = false, copyable = false, wrap = false, className }: CodeBlockProps) {
  const lines = (typeof code === 'string' ? code.split('\n') : code).map((line) => (typeof line === 'string' ? { text: line } : line))
  const diff = lines.some((line) => line.change)
  const copy = copyable && (
    <CopyButton
      text={lines.filter((line) => line.change !== 'removed').map((line) => line.text).join('\n')}
      className="-m-xs text-neutral-8 [&_svg]:size-[14px]"
    />
  )

  const body = (
    <div className={cx('min-w-0 flex-1', !wrap && 'overflow-x-auto')}>
      <pre className={cx('font-code text-xs leading-s tracking-m', !wrap && 'min-w-max')}>
        {lines.map((line, index) => (
          <div
            key={index}
            className={cx(
              'flex gap-s',
              title && 'px-m',
              (line.change || line.highlight) && 'bg-(--code-highlight,var(--neutral-3))',
              // A diff dims its unchanged lines next to the added ones.
              line.change === 'removed' ? 'text-neutral-6' : diff && !line.change ? 'text-neutral-8' : 'text-neutral-10',
            )}
          >
            {lineNumbers && <span className="w-[20px] shrink-0 text-right text-neutral-6 select-none">{index + 1}</span>}
            {diff && (
              <span className={cx('w-[10px] shrink-0 select-none', line.change === 'added' ? 'text-color-1' : 'text-neutral-6')}>
                {line.change === 'added' ? '+' : line.change === 'removed' ? '−' : ''}
                {line.change && <span className="sr-only">{line.change === 'added' ? ' added' : ' removed'}</span>}
              </span>
            )}
            <code className={wrap ? 'min-w-0 break-all whitespace-pre-wrap' : 'whitespace-pre'}>{line.text || ' '}</code>
          </div>
        ))}
      </pre>
    </div>
  )

  if (!title) {
    return (
      <div className={cx('flex items-start gap-m rounded-s bg-(--code-background,var(--neutral-2)) p-m', className)}>
        {body}
        {copy}
      </div>
    )
  }
  return (
    <figure className={cx('flex flex-col rounded-m bg-(--code-background,var(--neutral-2)) pb-s', className)}>
      <figcaption className="flex items-center justify-between gap-m px-m py-s">
        <span className="truncate font-code text-xs tracking-m text-neutral-7">{title}</span>
        {copy}
      </figcaption>
      {body}
    </figure>
  )
}
