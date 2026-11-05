import { useQuery, type QueryClient } from '@tanstack/react-query'
import { isApiError, type Schemas } from '~/api/client'
import { jobQuery } from '~/api/jobs'
import { KeyValue, KeyValueList } from '~/components/app/key-value'
import { LoadError, NotFound } from '~/components/app/load-error'
import { RunnerLabel } from '~/components/app/runner-label'
import { Skeleton } from '~/components/ui/skeleton'
import { formatDuration } from '~/lib/format'
import { useNow } from '~/lib/use-now'
import { DebugSandbox } from './debug-sandbox'
import { JobCharts } from './job-charts'
import { queueTime, runTime, trigger } from './job-format'
import { JobHeader } from './job-header'
import { JobLog } from './job-log'
import { shownStep } from './job-steps'
import { JobStepsList } from './job-steps-list'

/** Starts the request of the page, and returns the job for the title. */
export function prefetch(client: QueryClient, jobId: string) {
  // A failed load stays in the query cache, and the page shows it with a retry.
  return client.ensureQueryData(jobQuery(jobId)).catch(() => undefined)
}

type JobPageProps = {
  jobId: string
  /** The step in the URL, 0 for the whole log, or undefined for the default step. */
  step: number | undefined
}

/**
 * The "Job detail" design: header with re-run actions, facts of the run, the
 * debug sandbox of a debug run, steps, CPU and memory, and the log.
 */
export function JobPage({ jobId, step }: JobPageProps) {
  const query = useQuery({
    ...jobQuery(jobId),
    refetchInterval: ({ state }) => (state.data && awaitsHold(state.data) ? 2000 : false),
  })
  const now = useNow(1000)

  if (query.isPending) return <JobPageSkeleton />
  if (isApiError(query.error, 404)) {
    return (
      <NotFound title="Job not found" to="/jobs" link="All jobs">
        No job has this ID. Jobs older than the job retention are deleted.
      </NotFound>
    )
  }
  if (query.isError) {
    return <LoadError heading={1} what="the job" error={query.error} onRetry={() => void query.refetch()} retrying={query.isFetching} />
  }

  const job = query.data
  const shown = shownStep(job, step)
  return (
    <>
      <JobHeader job={job} />
      <JobFacts job={job} now={now} />
      <DebugSandbox job={job} now={now} />
      <div className="flex flex-col gap-xl xl:flex-row xl:items-start">
        <JobStepsList job={job} selected={shown} now={now} />
        <div className="@container flex min-w-0 grow basis-0 flex-col gap-xl">
          {job.started_at && <JobCharts job={job} now={now} />}
          <JobLog job={job} step={shown} now={now} />
        </div>
      </div>
    </>
  )
}

function JobFacts({ job, now }: { job: Schemas['Job']; now: number }) {
  const duration = runTime(job, now)
  const message = job.commit_message.split('\n')[0]
  return (
    // The facts sit closer to the title than the page gap.
    <KeyValueList className="-mt-m">
      <KeyValue label="Commit">
        <span className="block max-w-[48ch] truncate" title={job.commit_message || undefined}>
          {job.head_sha.slice(0, 7)} {message}
        </span>
      </KeyValue>
      <KeyValue label="Branch">{job.head_branch}</KeyValue>
      <KeyValue label="Runner">
        <RunnerLabel className="block leading-s text-neutral-10">{job.pool_label || job.labels.join(', ')}</RunnerLabel>
      </KeyValue>
      <KeyValue label="Server">{job.server_name || '—'}</KeyValue>
      <KeyValue label="Queued">
        <span className="tabular-nums">{formatDuration(queueTime(job, now))}</span>
      </KeyValue>
      <KeyValue label="Duration">
        <span className="tabular-nums">{duration === null ? '—' : formatDuration(duration)}</span>
      </KeyValue>
      <KeyValue label="Triggered">{trigger(job)}</KeyValue>
    </KeyValueList>
  )
}

/**
 * A finished debug job whose sandbox has no hold time yet. The scheduler sets
 * the hold after the job ends, and no event tells the page, so it asks again.
 */
function awaitsHold(job: Schemas['Job']): boolean {
  return job.state === 'completed' && job.ssh_command !== '' && job.hold_until === null
}

/** The page while the job loads: the shapes of the header, steps, charts, and log. */
export function JobPageSkeleton() {
  return (
    <div aria-busy className="flex flex-col gap-xxl">
      <p className="sr-only" role="status">
        Loading the job
      </p>
      <div className="flex flex-col gap-s" aria-hidden>
        <Skeleton className="my-[3px] h-[12px] w-[220px]" />
        <Skeleton className="my-[3px] h-[24px] w-[200px]" />
      </div>
      <div className="flex flex-col gap-xl xl:flex-row xl:items-start" aria-hidden>
        <div className="flex w-full shrink-0 flex-col gap-s xl:w-[280px]">
          {Array.from({ length: 6 }, (_, index) => (
            <Skeleton key={index} className="h-[18px] my-xs" />
          ))}
        </div>
        <div className="flex min-w-0 grow basis-0 flex-col gap-xl">
          <span className="block h-[160px] animate-pulse rounded-m bg-neutral-2" />
          <span className="block h-[360px] animate-pulse rounded-m bg-neutral-2" />
        </div>
      </div>
    </div>
  )
}
