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
  type DataTableColumn,
} from '~/components/app/data-table'
import { None } from '~/components/app/none'
import { RunnerLabel } from '~/components/app/runner-label'
import { SectionHeader, SectionLink } from '~/components/app/section-header'
import { jobStatus, StatusIcon } from '~/components/app/status-icon'
import { UsageBar } from '~/components/app/usage-bar'
import { formatDuration, formatRelativeTime } from '~/lib/format'
import { useNow } from '~/lib/use-now'
import { jobProgress } from './data'

type Job = Schemas['JobSummary']

type Column = {
  head?: string
  width: DataTableColumn
  align?: 'start' | 'end'
  /** Narrow tables drop the columns with drop 1 first, then 2, then 3 (useFittingColumns). */
  drop?: 1 | 2 | 3
  className?: string
  cell: (job: Job, now: number) => ReactNode
}

/** "Running": every running job with its progress against its usual run time and a ticking elapsed time. */
export function RunningJobs({ jobs }: { jobs: readonly Job[] }) {
  return (
    <section className="flex flex-col gap-s">
      <SectionHeader title="Running" count={jobs.length} />
      <JobTable aria-label="Running jobs" columns={runningColumns} jobs={jobs} empty="No jobs are running." />
    </section>
  )
}

/** "Recent jobs": the last completed jobs, with a link to all jobs. */
export function RecentJobs({ jobs }: { jobs: readonly Job[] }) {
  return (
    <section className="flex flex-col gap-s">
      <SectionHeader
        title="Recent jobs"
        action={
          <SectionLink to="/jobs" chevron>
            All jobs
          </SectionLink>
        }
      />
      <JobTable aria-label="Recent jobs" columns={recentColumns} jobs={jobs} empty="No job has finished yet." />
    </section>
  )
}

type JobTableProps = {
  'aria-label': string
  columns: readonly Column[]
  jobs: readonly Job[]
  empty: string
}

function JobTable({ columns, jobs, empty, ...props }: JobTableProps) {
  // Only the status column has no head.
  const fit = useFittingColumns(Object.fromEntries(columns.map((column) => [column.head ?? '', column])))
  const shown = columns.filter((column) => fit.shown[column.head ?? ''])
  const now = useNow(1000)
  return (
    <div ref={fit.ref}>
      <DataTable {...props} columns={fit.columns}>
        <DataTableHeader>
          {shown.map((column) => (
            <DataTableHead key={column.head ?? ''} align={column.align}>
              {column.head ?? <span className="sr-only">Status</span>}
            </DataTableHead>
          ))}
        </DataTableHeader>
        {jobs.length === 0 && <DataTableEmpty>{empty}</DataTableEmpty>}
        {jobs.map((job) => (
          <DataTableRow key={job.id}>
            {shown.map((column) => (
              <DataTableCell key={column.head ?? ''} align={column.align} className={column.className}>
                {column.cell(job, now)}
              </DataTableCell>
            ))}
          </DataTableRow>
        ))}
      </DataTable>
    </div>
  )
}

const jobColumns: Column[] = [
  {
    head: 'Job',
    width: 'grow',
    cell: (job) => {
      const workflow = job.workflow_file.split('/').pop()
      return (
        <RowLink to="/jobs/$jobId" params={{ jobId: job.id }}>
          <CellTitle
            title={job.job_name}
            detail={workflow ? `${job.repository} · ${workflow}` : job.repository}
            muted={jobStatus(job) === 'cancelled'}
          />
        </RowLink>
      )
    },
  },
  { head: 'Branch', width: 200, drop: 1, cell: (job) => <BranchRef branch={job.head_branch} /> },
  { head: 'Runner', width: 150, drop: 1, cell: (job) => (job.pool_label ? <RunnerLabel>{job.pool_label}</RunnerLabel> : <None />) },
  { head: 'Server', width: 120, drop: 2, cell: (job) => (job.server_name ? <span className="truncate">{job.server_name}</span> : <None />) },
]

const runningColumns: Column[] = [
  { width: 16, cell: () => <StatusIcon status="running" /> },
  ...jobColumns,
  {
    head: 'Progress',
    width: 96,
    drop: 3,
    cell: (job, now) => <UsageBar value={jobProgress(elapsed(job, now), job.expected_duration_ms)} label="Progress" className="w-full" />,
  },
  { head: 'Elapsed', width: 56, align: 'end', className: 'whitespace-nowrap', cell: (job, now) => formatDuration(elapsed(job, now)) },
]

const recentColumns: Column[] = [
  { width: 16, cell: (job) => <StatusIcon status={jobStatus(job)} /> },
  ...jobColumns,
  { head: 'Duration', width: 96, align: 'end', drop: 3, className: 'whitespace-nowrap', cell: (job) => formatDuration(job.duration_ms) },
  {
    head: 'Finished',
    width: 56,
    align: 'end',
    className: 'whitespace-nowrap text-neutral-7',
    cell: (job, now) => job.finished_at && formatRelativeTime(job.finished_at, now),
  },
]

function elapsed(job: Job, now: number): number {
  return now - Date.parse(job.started_at ?? job.queued_at)
}
