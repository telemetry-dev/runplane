import { useQuery } from '@tanstack/react-query'
import { TerminalIcon } from 'lucide-react'
import { lazy, Suspense } from 'react'
import type { Schemas } from '~/api/client'
import { CodeBlock } from '~/components/app/code-block'
import { settingsQuery } from '~/components/pages/settings/settings-api'
import { Button } from '~/components/ui/button'
import { Dialog, DialogPopup, DialogTrigger } from '~/components/ui/dialog'
import { Spinner } from '~/components/ui/spinner'
import { formatDuration } from '~/lib/format'

// xterm is large; load it when a terminal opens.
const SandboxTerminal = lazy(() => import('./sandbox-terminal'))

/**
 * The banner of a debug run ("Re-run with SSH"): how long the sandbox stays,
 * the ssh command, and a browser terminal. It shows while the sandbox runs the
 * job and during the hold time after it.
 */
export function DebugSandbox({ job, now }: { job: Schemas['Job']; now: number }) {
  const settings = useQuery({ ...settingsQuery, enabled: job.ssh_command !== '' }).data
  const remaining = job.hold_until ? Date.parse(job.hold_until) - now : null
  if (!job.ssh_command) return null
  if (job.state === 'completed' && remaining !== null && remaining <= 0) return null

  return (
    <section
      aria-labelledby="debug-sandbox"
      className="flex flex-col gap-m rounded-m bg-neutral-2 p-l [--code-background:var(--neutral-1)] lg:flex-row lg:items-center lg:gap-xl"
    >
      <div className="flex min-w-0 flex-col gap-xxs lg:w-[240px] lg:shrink-0">
        <h2 id="debug-sandbox" className="text-m font-heavy tracking-l text-neutral-10">
          Debug sandbox
        </h2>
        <p className="text-s text-neutral-8">
          {remaining !== null && remaining > 0 ? (
            <>
              Stops in <span className="tabular-nums">{formatDuration(remaining)}</span>
            </>
          ) : settings ? (
            `Stays ${settings.debug_hold_minutes} minutes after the job`
          ) : (
            'Stays after the job'
          )}
        </p>
      </div>
      <CodeBlock code={job.ssh_command} copyable className="min-w-0 grow" />
      <Dialog
        onOpenChange={(open, details) => {
          // Escape is a key of the shell (vim, less), not a request to close.
          if (!open && details.reason === 'escape-key' && details.event.target instanceof Element && details.event.target.closest('.xterm')) {
            details.cancel()
          }
        }}
      >
        <DialogTrigger render={<Button variant="primary" className="self-start lg:self-auto" />}>
          <TerminalIcon aria-hidden />
          Open terminal
        </DialogTrigger>
        <DialogPopup title={`Terminal · ${job.job_name}`} width={880}>
          <Suspense fallback={<Spinner label="Loading the terminal" />}>
            <SandboxTerminal sandboxId={job.sandbox_id} />
          </Suspense>
        </DialogPopup>
      </Dialog>
    </section>
  )
}
