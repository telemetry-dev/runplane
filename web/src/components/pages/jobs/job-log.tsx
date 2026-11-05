import { useQuery } from '@tanstack/react-query'
import type { ReactNode } from 'react'
import { errorMessage, type Schemas } from '~/api/client'
import { useFollowedText } from '~/api/followed-text'
import { followJobLog, logQuery } from '~/api/jobs'
import { LogViewer } from '~/components/app/log-viewer'
import { stepStatus } from '~/components/app/status-icon'
import { TextButton } from '~/components/app/text-button'
import { formatDuration } from '~/lib/format'
import { noLogReason, runTime, stepTime } from './job-format'
import { withoutRunGroups } from './job-steps'

type JobLogProps = {
  job: Schemas['Job']
  /** Step number, 0 for the whole log. */
  step: number
  now: number
}

/** The log panel of Job detail. A running job streams its log and keeps the newest line in view. */
export function JobLog({ job, step, now }: JobLogProps) {
  const selected = job.steps.find((item) => item.number === step)
  const live = job.log_state === 'live'
  const log = useJobLog(job, step)
  const time = selected ? stepTime(selected, now) : runTime(job, now)

  let empty: ReactNode = 'No log output.'
  if (job.log_state === 'none') empty = noLogReason(job)
  else if (job.log_state === 'expired') empty = 'This log is older than the log retention and was deleted.'
  else if (selected && stepStatus(selected) === 'skipped') empty = 'This step was skipped.'
  else if (log.error) {
    empty = (
      <span role="alert">
        Could not load the log: {errorMessage(log.error)}{' '}
        <TextButton onClick={log.retry}>Try again</TextButton>
      </span>
    )
  } else if (log.text === undefined) empty = 'Loading the log…'

  return (
    <LogViewer
      // Folded groups and the search belong to one log; start fresh for another step.
      key={step}
      log={log.text === undefined ? '' : step ? withoutRunGroups(log.text) : log.text}
      title={selected?.name ?? 'Full log'}
      meta={time === null ? undefined : formatDuration(time)}
      follow={live}
      // Without a log, LogViewer disables its download button.
      downloadUrl={job.log_state === 'stored' || live ? `/api/jobs/${job.id}/log` : undefined}
      downloadName={`${job.repository.split('/').pop()}-${job.job_name}-${job.run_number}.log`.replace(/[^\w.-]+/g, '-')}
      empty={empty}
    />
  )
}

type JobLogText = {
  /** undefined until the first bytes arrive. */
  text: string | undefined
  error: unknown
  retry: () => void
}

/**
 * The log of a step, or of the whole job for step 0. A stored log loads once; a
 * live log streams while the job runs.
 */
function useJobLog(job: Schemas['Job'], step: number): JobLogText {
  const stored = useQuery({ ...logQuery(job.id, step), enabled: job.log_state === 'stored' })
  // A step that has finished has all its lines, so its stream may end for good.
  const running = !job.steps.find((item) => item.number === step)?.completed_at
  const live = useFollowedText(followJobLog(job.id, step), { enabled: job.log_state === 'live', reconnect: running })
  // When a live log ends, its text stays on screen until the stored copy loads.
  if (job.log_state === 'stored' && (stored.data !== undefined || live.text === undefined)) {
    return { text: stored.data, error: stored.error, retry: () => void stored.refetch() }
  }
  return live
}
