import { useState, type ReactNode } from 'react'
import { StatusIcon } from '~/components/app/status-icon'
import { Spinner } from '~/components/ui/spinner'
import { installState, type Server } from './server-info'

type InstallProgressProps = {
  /** The new server, once runplane knows it. */
  server: Server | undefined
  /** The install token expired before a server connected with it. */
  expired?: boolean
}

/**
 * The live row under the install method of "Add server": a spinner while
 * runplane waits for the new server, then a check once the agent connects. The
 * install steps the server reports (status_detail) collect below it.
 */
export function InstallProgress({ server, expired = false }: InstallProgressProps) {
  const state = installState(server)
  const steps = useSteps(server?.status_detail)

  let icon: ReactNode = <Spinner />
  let title: ReactNode = 'Waiting for the server to connect'
  if (state === 'connected' && server) {
    icon = <StatusIcon status="succeeded" />
    title = `${server.name} is connected`
  } else if (state === 'failed') {
    icon = <StatusIcon status="failed" />
    title = 'The install failed'
  } else if (expired) {
    icon = <StatusIcon status="cancelled" />
    title = 'The token expired before a server connected'
  }

  return (
    // The dividers get their own pixels of padding, as borders would.
    <div
      role="status"
      className="flex flex-col gap-xxs py-[calc(var(--space-s)+1px)] shadow-[inset_0_1px_0_var(--neutral-4),inset_0_-1px_0_var(--neutral-4)]"
    >
      <div className="flex items-center gap-s">
        {icon}
        <span className="min-w-0 text-s font-medium text-neutral-10">{title}</span>
      </div>
      {steps.length > 0 && (
        <ol aria-label="Install steps" className="flex flex-col pl-[calc(16px+var(--space-s))] text-xs text-neutral-7">
          {steps.map((step, index) => (
            <li key={index} className={index === steps.length - 1 ? 'text-neutral-8' : undefined}>
              {step}
            </li>
          ))}
        </ol>
      )}
    </div>
  )
}

/** Each status_detail the server has reported, in order, so earlier steps stay visible. */
function useSteps(detail: string | undefined): string[] {
  const [steps, setSteps] = useState<string[]>([])
  if (detail && steps.at(-1) !== detail) setSteps([...steps, detail])
  return steps
}
