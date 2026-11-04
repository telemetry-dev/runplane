import { keepPreviousData, useQuery } from '@tanstack/react-query'
import { $api, errorMessage } from '~/api/client'
import { Button } from '~/components/ui/button'
import { PageHeader } from '~/components/ui/page-header'
import { SegmentedControl } from '~/components/ui/segmented'
import { Spinner } from '~/components/ui/spinner'
import { hasNoJobs, ranges, type Overview, type Range } from './data'
import { RecentJobs, RunningJobs } from './job-tables'
import { JobsChart } from './jobs-chart'
import { NoJobs } from './no-jobs'
import { ServerList, ServersCard } from './servers'
import { OverviewStats, statsGrid } from './stats'

export function overviewQuery(range: Range) {
  return $api.queryOptions('get', '/api/overview', { params: { query: { range } } })
}

const rangeOptions = ranges.map((range) => ({ value: range, label: range }))

type OverviewPageProps = {
  range: Range
  onRangeChange: (range: Range) => void
}

/**
 * The Overview page. Events keep it current: job.state and server.state refetch
 * it, and host.stats moves the server CPU bars. While another range loads, the
 * page keeps the last one and shows a spinner by the range control.
 */
export function OverviewPage({ range, onRangeChange }: OverviewPageProps) {
  const query = useQuery({ ...overviewQuery(range), placeholderData: keepPreviousData })
  const { data } = query

  return (
    <>
      <PageHeader
        title="Overview"
        actions={
          <>
            {query.isPlaceholderData && <Spinner size={14} label="Loading" />}
            <SegmentedControl aria-label="Time range" value={range} onValueChange={onRangeChange} options={rangeOptions} />
          </>
        }
      />
      {/* A failed refetch keeps the data on screen; a failed first load or range change shows the error. */}
      {query.isError && (!data || query.isPlaceholderData) ? (
        <LoadError error={query.error} onRetry={() => void query.refetch()} />
      ) : data ? (
        <OverviewBody overview={data} />
      ) : (
        <OverviewSkeleton />
      )}
    </>
  )
}

function OverviewBody({ overview }: { overview: Overview }) {
  if (hasNoJobs(overview)) {
    return (
      <>
        <OverviewStats overview={overview} />
        <NoJobs />
        <ServerList servers={overview.servers} />
      </>
    )
  }
  return (
    <>
      <OverviewStats overview={overview} />
      <RunningJobs jobs={overview.running} />
      <div className="flex flex-col gap-xl xl:flex-row">
        <JobsChart range={overview.range} buckets={overview.buckets} />
        <ServersCard servers={overview.servers} />
      </div>
      <RecentJobs jobs={overview.recent} />
    </>
  )
}

function OverviewSkeleton() {
  const block = 'animate-pulse rounded-m bg-neutral-2'
  return (
    <div role="status" aria-label="Loading the overview" className="flex flex-col gap-xxl">
      <div className={statsGrid}>
        {Array.from({ length: 5 }, (_, index) => (
          <div key={index} className={`${block} h-[56px]`} />
        ))}
      </div>
      <div className={`${block} h-[263px]`} />
    </div>
  )
}

function LoadError({ error, onRetry }: { error: unknown; onRetry: () => void }) {
  return (
    <div role="alert" className="flex flex-col items-start gap-s">
      <h2 className="type-section text-neutral-10">The overview did not load</h2>
      <p className="text-s text-neutral-8">{errorMessage(error)}</p>
      <Button className="mt-s" onClick={onRetry}>
        Try again
      </Button>
    </div>
  )
}
