import { Stat } from '~/components/app/stat'
import { statValues, type Overview } from './data'

export const statsGrid = 'grid grid-cols-2 gap-xl sm:grid-cols-3 lg:grid-cols-5'

/** The stats row: jobs, queue time, failures, and cache hits of the range, and the vCPUs in use now. */
export function OverviewStats({ overview }: { overview: Overview }) {
  const values = statValues(overview)
  return (
    <div className={statsGrid}>
      <Stat label="Jobs" value={values.jobs} />
      <Stat label="vCPU in use" value={overview.stats.vcpu_in_use} total={overview.stats.vcpu_total} />
      <Stat label="Median queue time" value={values.medianQueue} />
      <Stat label="Failure rate" value={values.failureRate} />
      <Stat label="Cache hit rate" value={values.cacheHitRate} />
    </div>
  )
}
