import { useQuery } from '@tanstack/react-query'
import { useEffect, useState, type ReactNode } from 'react'
import { api, errorMessage, type Schemas } from '~/api/client'
import { abortOnPageHide, usePageShows } from '~/api/open-requests'
import { LogViewer } from '~/components/app/log-viewer'
import { stepStatus } from '~/components/app/status-icon'
import { formatDuration } from '~/lib/format'
import { noLogReason, runTime, stepTime } from './job-format'
import { logQuery } from './job-queries'
import { withoutRunGroups } from './job-steps'
import { TextButton } from './text-button'

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
 * live log streams from GET /api/jobs/{id}/log?follow=1 while the job runs.
 */
function useJobLog(job: Schemas['Job'], step: number): JobLogText {
  const stored = useQuery({ ...logQuery(job.id, step), enabled: job.log_state === 'stored' })
  // A step that has finished has all its lines, so its stream may end for good.
  const running = !job.steps.find((item) => item.number === step)?.completed_at
  const live = useLiveLog(job.id, step, job.log_state === 'live', running)
  // When a live log ends, its text stays on screen until the stored copy loads.
  const liveText = live.key === `${job.id}/${step}` ? live.text : undefined
  if (job.log_state === 'stored' && (stored.data !== undefined || liveText === undefined)) {
    return { text: stored.data, error: stored.error, retry: () => void stored.refetch() }
  }
  return { text: liveText, error: live.error, retry: live.retry }
}

type LiveLog = { key: string; text: string | undefined; error: unknown }

/**
 * Streams a live log. When `reconnect` is set, a stream that ends or fails
 * opens again after 3 seconds, until the job ends and `enabled` turns off;
 * the text on screen stays until the new stream sends its lines.
 */
function useLiveLog(id: string, step: number, enabled: boolean, reconnect: boolean) {
  const [log, setLog] = useState<LiveLog>({ key: '', text: undefined, error: null })
  const [attempt, setAttempt] = useState(0)
  const shows = usePageShows()

  useEffect(() => {
    if (!enabled) return
    const key = `${id}/${step}`
    const controller = new AbortController()
    const release = abortOnPageHide(controller)
    let text = ''
    // LogViewer parses the whole log on each change, so batch the chunks of a burst.
    let timer: ReturnType<typeof setTimeout> | undefined
    let again: ReturnType<typeof setTimeout> | undefined
    const show = () => {
      timer = undefined
      setLog({ key, text, error: null })
    }
    const retry = () => {
      if (reconnect) again = setTimeout(() => setAttempt((count) => count + 1), 3000)
    }
    setLog((previous) => (previous.key === key ? previous : { key, text: undefined, error: null }))

    async function stream() {
      const { data } = await api.GET('/api/jobs/{id}/log', {
        params: { path: { id }, query: { step: step || undefined, follow: true } },
        parseAs: 'stream',
        signal: controller.signal,
      })
      if (!data) return
      const reader = data.pipeThrough(new TextDecoderStream()).getReader()
      for (;;) {
        const { value, done } = await reader.read()
        if (done) break
        text += value
        timer ??= setTimeout(show, 200)
      }
      clearTimeout(timer)
      show()
    }

    stream().then(retry, (error: unknown) => {
      if (controller.signal.aborted) return
      setLog((previous) => ({ key, text: text || (previous.key === key ? previous.text : undefined), error }))
      retry()
    })
    return () => {
      release()
      controller.abort()
      clearTimeout(timer)
      clearTimeout(again)
    }
  }, [id, step, enabled, reconnect, attempt, shows])

  return { ...log, retry: () => setAttempt((count) => count + 1) }
}
