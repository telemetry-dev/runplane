import type { Schemas } from '~/api/client'
import { Spinner } from '~/components/ui/spinner'

type SandboxState = Schemas['Sandbox']['state']

/**
 * State glyph of a runner (sandbox) in a 16px slot: the spinner while it
 * starts, the filled dot of an online server while busy, and a ring while idle
 * or stopping. It is decorative; the row names the state in words next to it.
 */
export function RunnerStateIcon({ state }: { state: SandboxState }) {
  if (state === 'starting') return <Spinner />
  return (
    <svg width={16} height={16} viewBox="0 0 16 16" aria-hidden className="shrink-0">
      {state === 'busy' ? (
        <circle cx="8" cy="8" r="4" fill="var(--neutral-10)" />
      ) : (
        <circle cx="8" cy="8" r="3.5" fill="none" stroke={state === 'idle' ? 'var(--neutral-8)' : 'var(--neutral-6)'} strokeWidth="1.5" />
      )}
    </svg>
  )
}

export const sandboxStateNames: Record<SandboxState, string> = {
  starting: 'Starting',
  idle: 'Idle',
  busy: 'Busy',
  stopping: 'Stopping',
  gone: 'Gone',
  failed: 'Failed',
}
