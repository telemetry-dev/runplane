import { useQuery } from '@tanstack/react-query'
import { Link, useParams, useSearch } from '@tanstack/react-router'
import { errorMessage, isApiError, type Schemas } from '~/api/client'
import { jobQuery } from '~/api/jobs'
import { KeyValue, KeyValueList } from '~/components/app/key-value'
import { RunnerLabel } from '~/components/app/runner-label'
import { Button, buttonClassName } from '~/components/ui/button'
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

/**
 * The "Job detail" design: header with re-run actions, facts of the run, the
 * debug sandbox of a debug run, steps, CPU and memory, and the log.
 */
export function JobPage() {
  const { jobId } = useParams({ from: '/_app/jobs/$jobId' })
  const search = useSearch({ from: '/_app/jobs/$jobId' })
  const query = useQuery({
    ...jobQuery(jobId),
    refetchInterval: ({ state }) => (state.data && awaitsHold(state.data) ? 2000 : false),
  })
  const now = useNow(1000)

  if (query.isPending) return <JobPageSkeleton />
  if (query.isError) return <JobError error={query.error} onRetry={() => void query.refetch()} />

  const job = query.data
  const step = shownStep(job, search)
  return (
    <>
      <JobHeader job={job} />
      <JobFacts job={job} now={now} />
      <DebugSandbox job={job} now={now} />
      <div className="flex flex-col gap-xl xl:flex-row xl:items-start">
        <JobStepsList steps={job.steps} selected={step} linked={job.log_state === 'stored'} now={now} />
        <div className="@container flex min-w-0 grow basis-0 flex-col gap-xl">
          {job.started_at && <JobCharts job={job} now={now} />}
          <JobLog job={job} step={step} now={now} />
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

function JobError({ error, onRetry }: { error: unknown; onRetry: () => void }) {
  const missing = isApiError(error, 404)
  return (
    <div role="alert" className="flex flex-col items-start gap-m">
      <h1 className="type-page-title text-neutral-10">{missing ? 'Job not found' : 'Could not load the job'}</h1>
      <p className="text-s text-neutral-8">
        {missing ? 'No job has this ID. Jobs older than the job retention are deleted.' : errorMessage(error)}
      </p>
      {missing ? (
        <Link to="/jobs" className={buttonClassName('secondary')}>
          All jobs
        </Link>
      ) : (
        <Button onClick={onRetry}>Try again</Button>
      )}
    </div>
  )
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
