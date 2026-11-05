import { errorMessage } from '~/api/client'
import type { Doctor } from '~/api/servers'
import { DataTable, DataTableEmpty } from '~/components/app/data-table'
import { DoctorChecks } from '~/components/app/doctor-checks'
import { Button } from '~/components/ui/button'
import { Spinner } from '~/components/ui/spinner'
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
        <DoctorChecks checks={result.checks} running={running} />
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
