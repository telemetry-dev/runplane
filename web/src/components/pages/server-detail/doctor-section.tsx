import { errorMessage } from '~/api/client'
import type { Doctor } from '~/api/servers'
import { DataTable, DataTableEmpty } from '~/components/app/data-table'
import { CheckIcon } from '~/components/app/status-icon'
import { FixCommand } from '~/components/pages/servers/fix-command'
import { sortChecks, type Check } from '~/components/pages/servers/server-info'
import { Button } from '~/components/ui/button'
import { Spinner } from '~/components/ui/spinner'
import { cx } from '~/lib/cx'
import { formatTimeAgo } from '~/lib/format'
import { useNow } from '~/lib/use-now'
import { canRunDoctor, type Server } from './server-format'

type DoctorSectionProps = {
  server: Server
  admin: boolean
  doctor: Doctor
}

/** The doctor checks of the server, problems first. A run shows a spinner in place of each icon. */
export function DoctorSection({ server, admin, doctor }: DoctorSectionProps) {
  const now = useNow(30_000)
  const { result, running } = doctor
  return (
    <section className="flex flex-col gap-s">
      <h2 className="flex min-h-[22px] flex-wrap items-baseline gap-x-xs text-m font-heavy tracking-l text-neutral-10">
        Doctor
        {result?.checked_at && <span className="font-regular text-neutral-6">checked {formatTimeAgo(result.checked_at, now)}</span>}
      </h2>
      {doctor.error ? (
        <p role="alert" className="text-s text-neutral-8">
          Could not run the doctor. {errorMessage(doctor.error)}
        </p>
      ) : null}
      {result && result.checks.length > 0 ? (
        <ul
          aria-label="Doctor checks"
          aria-busy={running || undefined}
          className="relative overflow-hidden rounded-m after:pointer-events-none after:absolute after:inset-0 after:rounded-[inherit] after:shadow-[inset_0_0_0_1px_var(--neutral-4)]"
        >
          {sortChecks(result.checks).map((check) => (
            <CheckRow key={check.id} check={check} running={running} />
          ))}
        </ul>
      ) : (
        <DataTable aria-label="Doctor checks" columns={['grow']}>
          <DataTableEmpty>
            {running ? (
              <span role="status" className="inline-flex items-center gap-s">
                <Spinner />
                Running the checks
              </span>
            ) : result ? (
              'The doctor has no checks for this server.'
            ) : result === null ? (
              <span className="inline-flex flex-col items-center gap-m">
                Run the doctor to check this server.
                {admin && (
                  <Button disabled={!canRunDoctor(server)} onClick={doctor.run}>
                    Run doctor
                  </Button>
                )}
              </span>
            ) : (
              <span role="status" className="inline-flex items-center gap-s">
                <Spinner />
                Loading the last result
              </span>
            )}
          </DataTableEmpty>
        </DataTable>
      )}
    </section>
  )
}

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
      <span className="flex min-w-0 grow flex-col gap-xxs">
        <span className={problem ? 'text-neutral-10' : 'text-neutral-7'}>{check.detail}</span>
        {problem && check.fix && (
          <span className="text-neutral-8">
            <span className="mr-xs font-medium text-neutral-10">Fix</span>
            {check.fix}
          </span>
        )}
        {problem && check.command && <FixCommand command={check.command} className="mt-xxs self-start bg-neutral-3" />}
      </span>
    </li>
  )
}
