import { useQuery, useQueryClient } from '@tanstack/react-query'
import { Link } from '@tanstack/react-router'
import { useEffect, useEffectEvent } from 'react'
import type { Schemas } from '~/api/client'
import { useLiveEvents } from '~/api/events'
import { statusQuery } from '~/api/session'
import { DataTable, DataTableCell, DataTableEmpty, DataTableHead, DataTableHeader, DataTableRow, useFittingColumns } from '~/components/app/data-table'
import { buttonClassName } from '~/components/ui/button'
import { GithubIcon } from '~/components/ui/icons'
import { Spinner } from '~/components/ui/spinner'
import { formatCount, formatMegabytes } from '~/lib/format'
import { ExternalTextLink } from './parts'

// Parts of the "Setup · First job" design.

/** One row for each account that the app is installed on, as "kestrel-hq connected · 12 repositories". */
export function ConnectedAccounts({ installations }: { installations: Schemas['Installation'][] }) {
  if (installations.length === 0) {
    return (
      <div className={accountRow}>
        <GithubIcon size={16} aria-hidden />
        <p className="min-w-0 flex-1 text-s text-neutral-7">No GitHub account is connected yet.</p>
        <Link to="/setup/github" className="shrink-0 rounded-xs text-s font-medium text-neutral-7 transition-colors hover:text-neutral-10">
          Connect GitHub
        </Link>
      </div>
    )
  }
  return (
    <ul aria-label="Connected accounts" className="flex flex-col gap-xs">
      {installations.map((installation) => (
        <li key={installation.id} className={accountRow}>
          <GithubIcon size={16} aria-hidden />
          <p className="flex min-w-0 flex-1 flex-wrap items-baseline gap-x-xs text-s">
            <span className="font-medium text-neutral-10">{installation.account_login}</span>
            <span className="text-neutral-7">
              {installation.suspended ? 'suspended' : 'connected'} · {formatCount(installation.repository_count)}{' '}
              {installation.repository_count === 1 ? 'repository' : 'repositories'}
            </span>
          </p>
          <ExternalTextLink
            href={installation.html_url}
            aria-label={`Manage access of ${installation.account_login} on GitHub (opens in a new tab)`}
          >
            Manage access
          </ExternalTextLink>
        </li>
      ))}
    </ul>
  )
}

const accountRow = 'flex items-center gap-s rounded-s bg-neutral-2 px-m py-s text-neutral-10'

/** The change to a workflow that moves a job to runplane. */
export function workflowDiff(label: string) {
  return [
    'jobs:',
    '  test:',
    { text: '    runs-on: ubuntu-latest', change: 'removed' },
    { text: `    runs-on: ${label}`, change: 'added' },
    '    steps:',
    '      - uses: actions/checkout@v4',
  ] as const
}

/** "Labels you can use": the runs-on label, size, and arch of each pool. */
export function LabelTable({ pools }: { pools: Schemas['Pool'][] }) {
  const fit = useFittingColumns({ label: 'grow', size: 140, arch: { width: 64, drop: 1 } })
  return (
    <section ref={fit.ref} className="flex flex-col gap-xs">
      <h2 className="text-s font-medium text-neutral-10">Labels you can use</h2>
      <DataTable aria-label="Labels you can use" columns={fit.columns} rowSpacing="xs">
        <DataTableHeader>
          <DataTableHead>Label</DataTableHead>
          <DataTableHead>Size</DataTableHead>
          {fit.shown.arch && <DataTableHead align="end">Arch</DataTableHead>}
        </DataTableHeader>
        {pools.map((pool) => (
          <DataTableRow key={pool.id}>
            <DataTableCell>
              <span className="truncate font-code text-xs leading-s text-neutral-10">{pool.label}</span>
            </DataTableCell>
            <DataTableCell>
              {pool.vcpu} vCPU · {formatMegabytes(pool.memory_mb)}
            </DataTableCell>
            {fit.shown.arch && <DataTableCell align="end">{pool.arch}</DataTableCell>}
          </DataTableRow>
        ))}
        {pools.length === 0 && (
          <DataTableEmpty>
            No labels yet.{' '}
            <Link to="/runners" className="font-medium text-neutral-10 underline underline-offset-2">
              Add a runner label
            </Link>
          </DataTableEmpty>
        )}
      </DataTable>
    </section>
  )
}

/** The waiting row. `onJob` runs when runplane has a job. */
export function JobWait({ onJob }: { onJob: () => void }) {
  useFirstJob(onJob)
  return (
    <div className="flex flex-wrap items-center justify-between gap-m rounded-m bg-neutral-2 p-m">
      <p role="status" className="flex items-center gap-s text-s font-medium text-neutral-10">
        <Spinner size={16} />
        Waiting for a job with a runplane label
      </p>
      <Link to="/" className={buttonClassName('secondary')}>
        Skip to dashboard
      </Link>
    </div>
  )
}

// runplane sees only jobs that ask for one of its labels, so any job means
// that the first job has arrived. A job event refetches the status, and the
// stream sends no past events, so each time it opens, the status tells
// whether a job came before.
function useFirstJob(onJob: () => void) {
  const queryClient = useQueryClient()
  const handle = useEffectEvent(onJob)
  const hasJobs = useQuery(statusQuery).data?.has_jobs
  useLiveEvents({ onOpen: () => void queryClient.invalidateQueries({ queryKey: statusQuery.queryKey }) })

  useEffect(() => {
    if (hasJobs) handle()
  }, [hasJobs])
}
