import { useQueryClient } from '@tanstack/react-query'
import { useCallback, useEffect, useRef, useState } from 'react'
import { $api, errorMessage, type Schemas } from '~/api/client'
import { putDoctor } from '~/api/servers'
import { CheckIcon } from '~/components/app/status-icon'
import { Button } from '~/components/ui/button'
import { Dialog, DialogClose, DialogFooter, DialogPopup } from '~/components/ui/dialog'
import { Spinner } from '~/components/ui/spinner'
import { FixCommand } from './fix-command'
import { sortChecks, type Check, type Server } from './server-info'

type DoctorDialogProps = { server: Server; open: boolean; onOpenChange: (open: boolean) => void }

/** Runs the doctor checks of a server when it opens, and lists them with problems first. */
export function DoctorDialog({ server, open, onOpenChange }: DoctorDialogProps) {
  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogPopup title={`Doctor · ${server.name}`} width={520}>
        <DoctorChecks serverId={server.id} />
      </DialogPopup>
    </Dialog>
  )
}

function DoctorChecks({ serverId }: { serverId: string }) {
  const client = useQueryClient()
  // The mutation clears its data when it runs again, so the last result stays here.
  const [result, setResult] = useState<Schemas['DoctorResult']>()
  const doctor = $api.useMutation('post', '/api/servers/{id}/doctor', {
    onSuccess: (data) => {
      setResult(data)
      putDoctor(client, serverId, data)
    },
  })
  const { mutate } = doctor
  const run = useCallback(() => mutate({ params: { path: { id: serverId } } }), [mutate, serverId])

  // The popup unmounts on close, so each opening runs the checks once. A ref
  // keeps StrictMode's second effect run from starting a second run.
  const started = useRef(false)
  useEffect(() => {
    if (started.current) return
    started.current = true
    run()
  }, [run])

  return (
    <>
      <div role="status" aria-busy={doctor.isPending} className="flex flex-col gap-s">
        {doctor.isError && <p className="text-s text-neutral-8">Could not run the doctor. {errorMessage(doctor.error)}</p>}
        {result ? (
          <CheckList checks={result.checks} />
        ) : (
          !doctor.isError && (
            <p className="flex items-center gap-s text-s text-neutral-8">
              <Spinner />
              Running the checks
            </p>
          )
        )}
      </div>
      <DialogFooter>
        {/* Without a result the body shows the spinner, so the button only waits. */}
        <Button onClick={run} disabled={doctor.isPending} loading={doctor.isPending && result !== undefined}>
          {doctor.isError ? 'Try again' : 'Run again'}
        </Button>
        <DialogClose render={<Button variant="primary" />}>Done</DialogClose>
      </DialogFooter>
    </>
  )
}

function CheckList({ checks }: { checks: Check[] }) {
  if (checks.length === 0) return <p className="text-s text-neutral-8">The doctor has no checks for this server.</p>
  const problems = checks.filter((check) => check.status !== 'ok').length
  return (
    <div className="flex flex-col gap-s">
      <p className="text-s text-neutral-8">
        {problems === 0 ? `All ${checks.length} checks passed.` : `${problems} of ${checks.length} checks need attention.`}
      </p>
      <ul className="flex flex-col">
        {sortChecks(checks).map((check) => (
          <li key={check.id} className="flex gap-s py-s [&:not(:last-child)]:shadow-[inset_0_-1px_0_var(--neutral-4)]">
            {/* Centered on the first line of the check name. */}
            <span className="flex h-(--line-s) shrink-0 items-center">
              <CheckIcon status={check.status} />
            </span>
            <div className="flex min-w-0 flex-1 flex-col gap-xxs">
              <span className="text-s font-medium text-neutral-10">{check.name}</span>
              {check.detail && <span className="text-xs text-neutral-7">{check.detail}</span>}
              {check.status !== 'ok' && check.fix && <span className="text-xs text-neutral-10">{check.fix}</span>}
              {check.status !== 'ok' && check.command && <FixCommand command={check.command} className="mt-xxs" />}
            </div>
          </li>
        ))}
      </ul>
    </div>
  )
}
