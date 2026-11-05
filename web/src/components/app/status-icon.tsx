import type { ReactNode } from 'react'
import type { Schemas } from '~/api/client'
import { Spinner } from '~/components/ui/spinner'

/** A state of a job, or its result once it completes. */
export type JobStatus = 'queued' | 'running' | NonNullable<Schemas['JobSummary']['result']>

type StatusIconProps = {
  status: JobStatus
  /** Pixels. The designs use 14 in the steps list, 16 in tables, and 22 next to the Job detail title. */
  size?: number
  className?: string
}

/** The job and step state glyphs of the designs. Each has an accessible name ("Failed"). */
export function StatusIcon({ status, size = 16, className }: StatusIconProps) {
  if (status === 'running') return <Spinner size={size} label={jobStatusNames.running} className={className} />
  return (
    <Glyph size={size} label={jobStatusNames[status]} className={className}>
      {glyphs[status](size)}
    </Glyph>
  )
}

export type CheckStatus = Schemas['Check']['status']

type CheckIconProps = {
  status: CheckStatus
  /** Pixels. Setup's host check uses 14, Server detail and the doctor dialog 16. */
  size?: number
  /** Draws a passed check as a plain neutral-7 tick, for long lists where most checks pass, as in Server detail. */
  quiet?: boolean
  className?: string
}

/** Result of a doctor check: the succeeded glyph, a ring with "!", or the failed glyph. Its name is Passed, Warning, or Failed. */
export function CheckIcon({ status, size = 16, quiet = false, className }: CheckIconProps) {
  return (
    <Glyph size={size} label={checkLabels[status]} className={className}>
      {checkGlyph(status, size, quiet)}
    </Glyph>
  )
}

function Glyph({ size, label, className, children }: { size: number; label: string; className?: string; children: ReactNode }) {
  return (
    <svg width={size} height={size} viewBox="0 0 24 24" role="img" aria-label={label} className={className} style={{ flexShrink: 0 }}>
      {children}
    </svg>
  )
}

export const jobStatusNames: Record<JobStatus, string> = {
  queued: 'Queued',
  running: 'Running',
  succeeded: 'Succeeded',
  failed: 'Failed',
  cancelled: 'Cancelled',
  skipped: 'Skipped',
}

const ring = { fill: 'none', strokeWidth: 1.5, strokeLinecap: 'round', strokeLinejoin: 'round' } as const

const glyphs: Record<Exclude<JobStatus, 'running'>, (size: number) => ReactNode> = {
  queued: () => <circle cx="12" cy="12" r="9.5" stroke="var(--neutral-7)" strokeDasharray="3 3" {...ring} />,
  succeeded: () => (
    <>
      <circle cx="12" cy="12" r="9.5" stroke="var(--neutral-10)" {...ring} />
      <path d="m8.5 12 2.5 2.5 4.5-5" stroke="var(--neutral-10)" {...ring} />
    </>
  ),
  failed: (size) => (
    <>
      <circle cx="12" cy="12" r="10.5" fill="var(--neutral-10)" />
      {/* The cross gets a heavier stroke at 14px so it stays readable. */}
      <path d="m15 9-6 6M9 9l6 6" fill="none" stroke="var(--neutral-1)" strokeWidth={size < 16 ? 2.25 : 2} strokeLinecap="round" />
    </>
  ),
  cancelled: () => (
    <>
      <circle cx="12" cy="12" r="9.5" stroke="var(--neutral-6)" {...ring} />
      <path d="m5.5 18.5 13-13" stroke="var(--neutral-6)" {...ring} />
    </>
  ),
  skipped: () => (
    <>
      <circle cx="12" cy="12" r="9.5" stroke="var(--neutral-6)" {...ring} />
      <path d="M8 12h8" stroke="var(--neutral-6)" {...ring} />
    </>
  ),
}

const checkLabels: Record<CheckStatus, string> = { ok: 'Passed', warn: 'Warning', fail: 'Failed' }

const tick = <path d="M20 6 9 17l-5-5" stroke="var(--neutral-7)" {...ring} />

function checkGlyph(status: CheckStatus, size: number, quiet: boolean): ReactNode {
  if (status === 'fail') return glyphs.failed(size)
  if (status === 'warn') return warning(size)
  return quiet ? tick : glyphs.succeeded(size)
}

// Heavier strokes at 14px keep the mark readable.
function warning(size: number) {
  const small = size < 16
  return (
    <>
      <circle cx="12" cy="12" r="9.5" fill="none" stroke="var(--neutral-10)" strokeWidth={small ? 1.75 : 1.5} />
      <path d="M12 7.5v5.5" fill="none" stroke="var(--neutral-10)" strokeWidth={small ? 2 : 1.75} strokeLinecap="round" />
      <circle cx="12" cy="16.5" r={small ? 1.25 : 1.125} fill="var(--neutral-10)" />
    </>
  )
}

/**
 * The icon status of a job from the API. `result` is set once the job
 * completes. An assigned job shows as running, because its sandbox is starting.
 */
export function jobStatus(job: Pick<Schemas['JobSummary'], 'state' | 'result'>): JobStatus {
  if (job.state === 'queued') return 'queued'
  if (job.state !== 'completed') return 'running'
  return job.result ?? 'failed'
}

/** The icon status of a GitHub step: `status` is queued, in_progress, or completed; `conclusion` is success, failure, … */
export function stepStatus(step: { status: string; conclusion?: string | null }): JobStatus {
  if (step.status === 'queued') return 'queued'
  if (step.status !== 'completed') return 'running'
  return stepConclusions[step.conclusion ?? ''] ?? 'failed'
}

const stepConclusions: Record<string, JobStatus> = {
  success: 'succeeded',
  failure: 'failed',
  cancelled: 'cancelled',
  skipped: 'skipped',
  neutral: 'succeeded',
}

type BuildState = Schemas['ImageBuild']['status']

/** The icon status of the build of an image on one server. */
export function buildStatus(state: BuildState): JobStatus {
  if (state === 'ready') return 'succeeded'
  if (state === 'pending') return 'queued'
  if (state === 'failed') return 'failed'
  return 'running'
}

export const buildStateNames: Record<BuildState, string> = {
  pending: 'Waiting',
  pulling: 'Pulling',
  building: 'Building',
  converting: 'Converting',
  ready: 'Ready',
  failed: 'Failed',
}
