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
import { RunnerLabel } from '~/components/app/runner-label'
import { jobStatus, StatusIcon } from '~/components/app/status-icon'
import { cx } from '~/lib/cx'
import { formatDuration, formatRelativeTime } from '~/lib/format'
import { jobOrigin, queueTime, runTime } from './job-format'
import { useNow } from './use-now'

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
          <SkeletonRows shown={shown} />
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
  const status = jobStatus(job)
  const queued = queueTime(job, now)
  const duration = runTime(job, now)
  return (
    <DataTableRow>
      <DataTableCell>
        <StatusIcon status={status} />
      </DataTableCell>
      <DataTableCell>
        <RowLink to="/jobs/$jobId" params={{ jobId: job.id }}>
          <CellTitle title={job.job_name} detail={jobOrigin(job)} muted={status === 'cancelled' || status === 'skipped'} />
        </RowLink>
      </DataTableCell>
      {shown.branch && (
        <DataTableCell>
          <BranchRef branch={job.head_branch} sha={job.head_sha} />
        </DataTableCell>
      )}
      {shown.runner && <DataTableCell>{job.pool_label ? <RunnerLabel>{job.pool_label}</RunnerLabel> : <Missing />}</DataTableCell>}
      {shown.server && <DataTableCell>{job.server_name ? <span className="truncate">{job.server_name}</span> : <Missing />}</DataTableCell>}
      {shown.queued && (
        <DataTableCell align="end" className={cx('tabular-nums', job.state === 'queued' && 'text-neutral-10')}>
          {formatDuration(queued)}
        </DataTableCell>
      )}
      <DataTableCell align="end" className={cx('tabular-nums', job.state !== 'completed' && 'text-neutral-10')}>
        {duration === null ? <Missing /> : formatDuration(duration)}
      </DataTableCell>
      {shown.started && (
        <DataTableCell align="end" className="text-neutral-7">
          <time dateTime={job.started_at ?? job.queued_at}>{formatRelativeTime(job.started_at ?? job.queued_at, now)}</time>
        </DataTableCell>
      )}
    </DataTableRow>
  )
}

function Missing() {
  return (
    <span className="text-neutral-6">
      <span aria-hidden>—</span>
      <span className="sr-only">None</span>
    </span>
  )
}

function SkeletonRows({ shown }: { shown: Shown }) {
  // Each bar sits on the line height of the text it stands for, so rows keep their height when the jobs arrive.
  return Array.from({ length: 8 }, (_, index) => (
    <DataTableRow key={index}>
      <DataTableCell>
        <span aria-hidden className="block size-[16px] animate-pulse rounded-full bg-neutral-3" />
      </DataTableCell>
      <DataTableCell className="flex-col items-start gap-0">
        <Bar className="my-[3px] h-[12px] w-[140px]" />
        <Bar className="my-[2px] h-[10px] w-[200px]" />
      </DataTableCell>
      {shown.branch && (
        <DataTableCell className="flex-col items-start gap-0">
          <Bar className="my-[3px] h-[12px] w-[120px]" />
          <Bar className="my-[2px] h-[10px] w-[56px]" />
        </DataTableCell>
      )}
      {shown.runner && (
        <DataTableCell>
          <Bar className="h-[10px] w-[96px]" />
        </DataTableCell>
      )}
      {shown.server && (
        <DataTableCell>
          <Bar className="h-[12px] w-[80px]" />
        </DataTableCell>
      )}
      {shown.queued && (
        <DataTableCell align="end">
          <Bar className="h-[12px] w-[24px]" />
        </DataTableCell>
      )}
      <DataTableCell align="end">
        <Bar className="h-[12px] w-[40px]" />
      </DataTableCell>
      {shown.started && (
        <DataTableCell align="end">
          <Bar className="h-[12px] w-[48px]" />
        </DataTableCell>
      )}
    </DataTableRow>
  ))
}

function Bar({ className }: { className: string }) {
  return <span aria-hidden className={cx('block animate-pulse rounded-xs bg-neutral-3', className)} />
}
