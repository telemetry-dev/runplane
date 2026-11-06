import { useEffect, useEffectEvent, useRef } from 'react'
import { errorMessage, type Schemas } from '~/api/client'
import { useDoctor } from '~/api/servers'
import { DoctorChecks } from '~/components/app/doctor-checks'
import { Button } from '~/components/ui/button'
import { Dialog, DialogClose, DialogFooter, DialogPopup } from '~/components/ui/dialog'
import { Spinner } from '~/components/ui/spinner'
import type { Server } from './server-info'

type DoctorDialogProps = { server: Server; open: boolean; onOpenChange: (open: boolean) => void }

/** Runs the doctor checks of a server when it opens, and lists them with problems first. */
export function DoctorDialog({ server, open, onOpenChange }: DoctorDialogProps) {
  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogPopup title={`Doctor · ${server.name}`} width={520}>
        <DoctorRun serverId={server.id} />
      </DialogPopup>
    </Dialog>
  )
}

function DoctorRun({ serverId }: { serverId: string }) {
  const doctor = useDoctor(serverId)
  const { result, running } = doctor
  const start = useEffectEvent(doctor.run)

  // The popup unmounts on close, so each opening runs the checks once. A ref
  // keeps StrictMode's second effect run from starting a second run.
  const started = useRef(false)
  useEffect(() => {
    if (started.current) return
    started.current = true
    start()
  }, [])

  return (
    <>
      <div role="status" aria-busy={running} className="flex flex-col gap-s">
        {doctor.error ? <p className="text-s text-neutral-8">Could not run the doctor. {errorMessage(doctor.error)}</p> : null}
        {result ? (
          <Summary checks={result.checks} />
        ) : (
          running && (
            <p className="flex items-center gap-s text-s text-neutral-8">
              <Spinner />
              Running the checks
            </p>
          )
        )}
        {result && result.checks.length > 0 && <DoctorChecks checks={result.checks} running={running} />}
      </div>
      <DialogFooter>
        {/* Without a result the body shows the spinner, so the button only waits. */}
        <Button onClick={doctor.run} disabled={running} loading={running && !!result}>
          {doctor.error ? 'Try again' : 'Run again'}
        </Button>
        <DialogClose render={<Button variant="primary" />}>Done</DialogClose>
      </DialogFooter>
    </>
  )
}

function Summary({ checks }: { checks: readonly Schemas['Check'][] }) {
  if (checks.length === 0) return <p className="text-s text-neutral-8">The doctor has no checks for this server.</p>
  const problems = checks.filter((check) => check.status !== 'ok').length
  return (
    <p className="text-s text-neutral-8">
      {problems === 0 ? `All ${checks.length} checks passed.` : `${problems} of ${checks.length} checks need attention.`}
    </p>
  )
}
