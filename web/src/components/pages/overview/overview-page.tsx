import { keepPreviousData, useQuery, type QueryClient } from '@tanstack/react-query'
import { useNavigate } from '@tanstack/react-router'
import { overviewQuery } from '~/api/overview'
import { LoadError } from '~/components/app/load-error'
import { PageHeader } from '~/components/ui/page-header'
import { SegmentedControl } from '~/components/ui/segmented'
import { Spinner } from '~/components/ui/spinner'
import { defaultRange, hasNoJobs, ranges, type Overview, type Range } from './data'
import { RecentJobs, RunningJobs } from './job-tables'
import { JobsChart } from './jobs-chart'
import { NoJobs } from './no-jobs'
import { ServerList, ServersCard } from './servers'
import { OverviewStats, statsGrid } from './stats'

const rangeOptions = ranges.map((range) => ({ value: range, label: range }))

/** Starts the request of the page, also on link hover; the page shows its own loading state. */
export function prefetch(client: QueryClient, range = defaultRange) {
  void client.prefetchQuery(overviewQuery(range))
}

/** The Overview page. While another range loads, the page keeps the last one and shows a spinner by the range control. */
export function OverviewPage({ range = defaultRange }: { range?: Range }) {
  const navigate = useNavigate()
  const query = useQuery({ ...overviewQuery(range), placeholderData: keepPreviousData })
  const { data } = query

  return (
    <>
      <PageHeader
        title="Overview"
        actions={
          <>
            {query.isPlaceholderData && <Spinner size={14} label="Loading" />}
            <SegmentedControl
              aria-label="Time range"
              value={range}
              onValueChange={(next) => void navigate({ to: '/', search: { range: next === defaultRange ? undefined : next }, replace: true })}
              options={rangeOptions}
            />
          </>
        }
      />
      {/* A failed refetch keeps the data on screen; a failed first load or range change shows the error. */}
      {query.isError && (!data || query.isPlaceholderData) ? (
        <LoadError heading={2} what="the overview" error={query.error} onRetry={() => void query.refetch()} retrying={query.isFetching} />
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
