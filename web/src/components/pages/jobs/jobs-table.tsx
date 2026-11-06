import type { ReactNode } from 'react'
import type { Schemas } from '~/api/client'
import { BranchRef } from '~/components/app/branch-ref'
import {
  CellTitle,
  DataTable,
  DataTableCell,
  DataTableEmpty,
  DataTableHead,
  DataTableHeader,
  DataTableRow,
  RowLink,
  useFittingColumns,
} from '~/components/app/data-table'
import { None } from '~/components/app/none'
import { RunnerLabel } from '~/components/app/runner-label'
import { SkeletonCell, SkeletonRows } from '~/components/app/skeleton-rows'
import { StatusIcon } from '~/components/app/status-icon'
import { cx } from '~/lib/cx'
import { formatDuration, formatRelativeTime } from '~/lib/format'
import { useNow } from '~/lib/use-now'
import { jobOrigin, mutedJob, queueTime, runTime } from './job-format'

// A narrow table drops Server first, then Queued, Runner, Branch, and Started.
const columns = {
  status: 16,
  job: 'grow',
  branch: { width: 200, drop: 4 },
  runner: { width: 150, drop: 3 },
  server: { width: 120, drop: 1 },
  queued: { width: 64, drop: 2 },
  duration: 72,
  started: { width: 72, drop: 5 },
} as const

type Shown = Record<keyof typeof columns, boolean>

type JobsTableProps = {
  /** null while the first page loads. */
  jobs: readonly Schemas['JobSummary'][] | null
  /** Replaces the rows when there are no jobs, for example "No jobs match these filters." */
  empty: ReactNode
  /** Dims the rows while the next filter result loads. */
  stale?: boolean
}

/** The Jobs table of the "Jobs" design. Queue and run times of waiting and running jobs tick each second. */
export function JobsTable({ jobs, empty, stale = false }: JobsTableProps) {
  const now = useNow(1000)
  const { ref, shown, columns: widths } = useFittingColumns(columns)
  return (
    <div ref={ref}>
      <DataTable aria-label="Jobs" columns={widths} className={cx('transition-opacity', stale && 'opacity-60')}>
        <DataTableHeader>
          <DataTableHead>
            <span className="sr-only">Status</span>
          </DataTableHead>
          <DataTableHead>Job</DataTableHead>
          {shown.branch && <DataTableHead>Branch</DataTableHead>}
          {shown.runner && <DataTableHead>Runner</DataTableHead>}
          {shown.server && <DataTableHead>Server</DataTableHead>}
          {shown.queued && <DataTableHead align="end">Queued</DataTableHead>}
          <DataTableHead align="end">Duration</DataTableHead>
          {shown.started && <DataTableHead align="end">Started</DataTableHead>}
        </DataTableHeader>
        {jobs === null ? (
          <SkeletonRows count={8}>
            <SkeletonCell shape="icon" />
            <SkeletonCell lines={[140, 200]} />
            {shown.branch && <SkeletonCell lines={[120, 56]} />}
            {shown.runner && <SkeletonCell lines={[96]} />}
            {shown.server && <SkeletonCell lines={[80]} />}
            {shown.queued && <SkeletonCell lines={[24]} align="end" />}
            <SkeletonCell lines={[40]} align="end" />
            {shown.started && <SkeletonCell lines={[48]} align="end" />}
          </SkeletonRows>
        ) : jobs.length === 0 ? (
          <DataTableEmpty>{empty}</DataTableEmpty>
        ) : (
          jobs.map((job) => <JobRow key={job.id} job={job} now={now} shown={shown} />)
        )}
      </DataTable>
    </div>
  )
}

function JobRow({ job, now, shown }: { job: Schemas['JobSummary']; now: number; shown: Shown }) {
  const queued = queueTime(job, now)
  const duration = runTime(job, now)
  return (
    <DataTableRow>
      <DataTableCell>
        <StatusIcon status={job.status} />
      </DataTableCell>
      <DataTableCell>
        <RowLink to="/jobs/$jobId" params={{ jobId: job.id }}>
          <CellTitle title={job.job_name} detail={jobOrigin(job)} muted={mutedJob(job.status)} />
        </RowLink>
      </DataTableCell>
      {shown.branch && (
        <DataTableCell>
          <BranchRef branch={job.head_branch} sha={job.head_sha} />
        </DataTableCell>
      )}
      {shown.runner && <DataTableCell>{job.pool_label ? <RunnerLabel>{job.pool_label}</RunnerLabel> : <None />}</DataTableCell>}
      {shown.server && <DataTableCell>{job.server_name ? <span className="truncate">{job.server_name}</span> : <None />}</DataTableCell>}
      {shown.queued && (
        <DataTableCell align="end" className={cx('tabular-nums', job.state === 'queued' && 'text-neutral-10')}>
          {formatDuration(queued)}
        </DataTableCell>
      )}
      <DataTableCell align="end" className={cx('tabular-nums', job.state !== 'completed' && 'text-neutral-10')}>
        {duration === null ? <None /> : formatDuration(duration)}
      </DataTableCell>
      {shown.started && (
        <DataTableCell align="end" className="text-neutral-7">
          <time dateTime={job.started_at ?? job.queued_at}>{formatRelativeTime(job.started_at ?? job.queued_at, now)}</time>
        </DataTableCell>
      )}
    </DataTableRow>
  )
}
