import type { Schemas } from '~/api/client'
import { Spinner } from '~/components/ui/spinner'
import { cx } from '~/lib/cx'
import { CopyButton } from './copy-button'
import { CheckIcon } from './status-icon'

type Check = Schemas['Check']

type DoctorChecksProps = {
  checks: readonly Check[]
  /** The checks run again: each icon is a spinner until the new result. */
  running?: boolean
  /** Draws the edge around the list. Without it, the list sits in a frame of the caller. */
  bordered?: boolean
}

/**
 * The doctor checks of a server, problems first. A problem shows how to fix it,
 * and the command that does the fix.
 */
export function DoctorChecks({ checks, running = false, bordered = true }: DoctorChecksProps) {
  return (
    <ul
      aria-label="Checks"
      aria-busy={running || undefined}
      className={cx(
        bordered && 'relative overflow-hidden rounded-m',
        bordered && 'after:pointer-events-none after:absolute after:inset-0 after:rounded-[inherit] after:shadow-[inset_0_0_0_1px_var(--neutral-4)]',
      )}
    >
      {sortChecks(checks).map((check) => (
        <CheckRow key={check.id} check={check} running={running} />
      ))}
    </ul>
  )
}

/** Problems first, failures before warnings; passed checks keep the doctor's order. */
export function sortChecks(checks: readonly Check[]): Check[] {
  return checks.toSorted((a, b) => checkRank[a.status] - checkRank[b.status])
}

const checkRank: Record<Check['status'], number> = { fail: 0, warn: 1, ok: 2 }

function CheckRow({ check, running }: { check: Check; running: boolean }) {
  const problem = check.status !== 'ok'
  return (
    <li
      className={cx(
        'flex items-start gap-m px-m text-s [&:not(:last-child)]:shadow-[inset_0_-1px_0_var(--neutral-4)]',
        problem ? 'bg-neutral-2 py-s' : 'py-xs',
      )}
    >
      {/* 16×18: centered on the first text line. */}
      <span className="flex h-(--line-s) w-[16px] shrink-0 items-center">
        {running ? <Spinner /> : <CheckIcon status={check.status} quiet />}
      </span>
      <span className={cx('w-[200px] max-w-[40%] shrink-0 text-neutral-10', problem && 'font-medium')}>{check.name}</span>
      <span className="flex min-w-0 grow flex-col gap-xxs break-words">
        <span className={problem ? 'text-neutral-10' : 'text-neutral-7'}>{check.detail}</span>
        {problem && check.fix && (
          <span className="text-neutral-8">
            <span className="mr-xs font-medium text-neutral-10">Fix</span>
            {check.fix}
          </span>
        )}
        {problem && check.command && <FixCommand command={check.command} />}
      </span>
    </li>
  )
}

function FixCommand({ command }: { command: string }) {
  return (
    <div className="mt-xxs flex items-start gap-s self-start rounded-s bg-neutral-3 py-xs pr-xs pl-s">
      <code className="min-w-0 flex-1 font-code text-xs leading-s tracking-m break-words text-neutral-10">{command}</code>
      {/* As tall as the code line, so the block keeps its height. */}
      <CopyButton text={command} className="-my-xxs p-xxs text-neutral-8 [&_svg]:size-[14px]" />
    </div>
  )
}
