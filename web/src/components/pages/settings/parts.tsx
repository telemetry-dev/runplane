import { Field as BaseField } from '@base-ui/react/field'
import { useId, type ReactNode } from 'react'
import { SectionHeader } from '~/components/app/section-header'
import { Button } from '~/components/ui/button'
import { Tooltip } from '~/components/ui/tooltip'
import { cx } from '~/lib/cx'

// The building blocks that all settings tabs share: the 720px column, sections,
// rows with a label and a control at the right edge, cards, and their loading
// and error states.

/**
 * The column of a settings tab. Inputs on the white page get the neutral-3
 * fill. Rows and forms inside follow its width: below 560px a row puts its
 * control under the label.
 */
export function SettingsColumn({ children }: { children: ReactNode }) {
  return <div className="@container flex w-full max-w-[720px] flex-col gap-xxl [--input-background:var(--neutral-3)]">{children}</div>
}

type SettingsSectionProps = {
  title: ReactNode
  count?: number
  action?: ReactNode
  /** A table follows the title: S spacing instead of the XS above row lists. */
  table?: boolean
  children: ReactNode
}

export function SettingsSection({ title, count, action, table = false, children }: SettingsSectionProps) {
  return (
    <section className={cx('flex flex-col', table ? 'gap-s' : 'gap-xs')}>
      <SectionHeader title={title} count={count} action={action} />
      {children}
    </section>
  )
}

/** Rows with a line between them and no outer edge. */
export function SettingRows({ children }: { children: ReactNode }) {
  // The divider under each row gets its own pixel of padding, as a border would.
  return (
    <div
      className={cx(
        'flex flex-col',
        '[&>*:not(:last-child)]:pb-[calc(var(--space-m)+1px)] [&>*:not(:last-child)]:shadow-[inset_0_-1px_0_var(--neutral-4)]',
      )}
    >
      {children}
    </div>
  )
}

type SettingRowProps = {
  label: ReactNode
  description: ReactNode
  /** The message of a failed save, under the control. */
  error?: string
  disabled?: boolean
  /** Top-aligned for tall controls, as the egress allowlist. */
  alignTop?: boolean
  /**
   * For a row with more than one control (Host reserve) or with buttons and
   * text instead of a form control. The label names the group, and each
   * control needs its own `aria-label`.
   */
  group?: boolean
  children?: ReactNode
}

/**
 * Label and description on the left, the control at the right edge. A single
 * control inside gets the label and description as its accessible name and
 * description, and the error as part of its description.
 */
export function SettingRow({ label, description, error, disabled, alignTop = false, group = false, children }: SettingRowProps) {
  const labelId = useId()
  const row = rowLayout(alignTop)
  const errorClass = 'max-w-[280px] text-xs text-error @min-[560px]:text-right'
  if (group) {
    return (
      <div role="group" aria-labelledby={labelId} className={row}>
        <RowText
          label={
            <span id={labelId} className={labelClass}>
              {label}
            </span>
          }
          description={<p className={descriptionClass}>{description}</p>}
        />
        <RowControl>
          {children}
          {error && (
            <p role="alert" className={errorClass}>
              {error}
            </p>
          )}
        </RowControl>
      </div>
    )
  }
  return (
    <BaseField.Root invalid={!!error} disabled={disabled} className={row}>
      <RowText
        label={<BaseField.Label className={labelClass}>{label}</BaseField.Label>}
        description={<BaseField.Description className={descriptionClass}>{description}</BaseField.Description>}
      />
      <RowControl>
        {children}
        {error && (
          <BaseField.Error match role="alert" className={errorClass}>
            {error}
          </BaseField.Error>
        )}
      </RowControl>
    </BaseField.Root>
  )
}

/** Side by side in a wide column; in a narrow one the control goes under the label. */
function rowLayout(alignTop = false) {
  return cx(
    'flex flex-col gap-s py-m @min-[560px]:flex-row @min-[560px]:justify-between @min-[560px]:gap-xl',
    !alignTop && '@min-[560px]:items-center',
  )
}

const labelClass = 'text-s font-medium text-neutral-10'
const descriptionClass = 'text-s text-pretty text-neutral-7'

function RowControl({ children }: { children: ReactNode }) {
  return <div className="flex shrink-0 flex-col items-start gap-xs @min-[560px]:items-end">{children}</div>
}

function RowText({ label, description }: { label: ReactNode; description: ReactNode }) {
  return (
    <div className="flex min-w-0 flex-1 flex-col gap-xxs">
      {label}
      {description}
    </div>
  )
}

/** A neutral-2 card inside a section, as "Upload to S3" and "Add image". Inputs on it are neutral-1. */
export function SettingsCard({ className, children }: { className?: string; children: ReactNode }) {
  return <div className={cx('flex flex-col rounded-m bg-neutral-2 p-l [--input-background:var(--neutral-1)]', className)}>{children}</div>
}

/** The title and help text of a SettingsCard. */
export function CardTitle({ title, children }: { title: ReactNode; children?: ReactNode }) {
  return (
    <div className="flex flex-col gap-xxs">
      <h3 className="text-s font-medium text-neutral-10">{title}</h3>
      {children && <p className="text-s text-pretty text-neutral-7">{children}</p>}
    </div>
  )
}

/** The small tag next to a name, as "Default" and "In use". */
export function Badge({ children }: { children: ReactNode }) {
  return <span className="shrink-0 rounded-xs bg-neutral-3 px-xs text-xs font-medium text-neutral-8">{children}</span>
}

/** A text button or link in a row, as "Release notes", "Copy link", and "Revoke". */
export const quietAction = cx(
  'inline-flex shrink-0 items-center gap-xs rounded-xs text-s font-medium whitespace-nowrap text-neutral-7 transition-colors',
  'hover:text-neutral-10 disabled:cursor-not-allowed disabled:text-neutral-5 [&_svg]:size-[14px]',
)

/**
 * Shows an error message in a tooltip on a status, as a failed image pull. The
 * trigger takes keyboard focus, and screen readers read the message with it.
 */
export function ErrorHint({ error, children }: { error: string; children: ReactNode }) {
  if (!error) return children
  return (
    <Tooltip content={error}>
      <span tabIndex={0} className="flex min-w-0 cursor-default items-center rounded-xs">
        {children}
        <span className="sr-only">: {error}</span>
      </span>
    </Tooltip>
  )
}

/** A loading placeholder; give it the size and corners of the text or control it stands for. */
export function Bar({ className }: { className: string }) {
  return <span aria-hidden className={cx('block animate-pulse bg-neutral-3', className)} />
}

/**
 * Placeholder rows with the height of real rows: a label line, a description
 * line, and a block in the shape of the control.
 */
export function SettingRowsSkeleton({ count = 2, control = 'input' }: { count?: number; control?: keyof typeof skeletonControls }) {
  return (
    <SettingRows>
      {Array.from({ length: count }, (_, index) => (
        <div key={index} className={rowLayout()}>
          <div className="flex flex-col gap-xxs">
            <Bar className="my-[3px] h-[12px] w-[120px] rounded-xs" />
            <Bar className="my-[3px] h-[12px] w-[260px] max-w-full rounded-xs" />
          </div>
          <Bar className={cx('shrink-0', skeletonControls[control])} />
        </div>
      ))}
    </SettingRows>
  )
}

const inputHeight = 'h-[calc(var(--line-s)+2*var(--space-xs))] rounded-s'
const skeletonControls = {
  input: cx(inputHeight, 'w-[280px]'),
  number: cx(inputHeight, 'w-[120px]'),
  switch: 'h-[18px] w-[32px] rounded-full',
}

/** In place of content whose data did not load. */
export function LoadError({ what = 'settings', onRetry, retrying }: { what?: string; onRetry: () => void; retrying: boolean }) {
  return (
    <div role="alert" className="flex items-center justify-between gap-m py-m text-s text-neutral-8">
      <p>Could not load {what}.</p>
      <Button onClick={onRetry} loading={retrying}>
        Retry
      </Button>
    </div>
  )
}

/** The note on tabs that a member can see but not change. */
export function ReadOnlyNote({ children = 'Only an admin can change these settings.' }: { children?: ReactNode }) {
  return <p className="text-s text-neutral-7">{children}</p>
}
