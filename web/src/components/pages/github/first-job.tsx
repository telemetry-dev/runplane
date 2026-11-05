import { useQueryClient } from '@tanstack/react-query'
import { Link } from '@tanstack/react-router'
import { ExternalLink } from 'lucide-react'
import { useEffect, useEffectEvent, useId, useState, type FormEvent } from 'react'
import { $api, errorMessage, isApiError, type Schemas } from '~/api/client'
import type { LiveEvent } from '~/api/events'
import { usePageShows } from '~/api/open-requests'
import { statusQuery } from '~/api/queries'
import { DataTable, DataTableCell, DataTableEmpty, DataTableHead, DataTableHeader, DataTableRow, useFittingColumns } from '~/components/app/data-table'
import { StatusIcon } from '~/components/app/status-icon'
import { FormAlert } from '~/components/pages/setup/parts'
import { Button, buttonClassName } from '~/components/ui/button'
import { Combobox, ComboboxInput, ComboboxItem, ComboboxPopup } from '~/components/ui/combobox'
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

type PullRequestFormProps = {
  /** owner/name of the repositories whose jobs runplane takes. */
  repositories: string[]
  label: string
  /** The permissions page of the GitHub App. An app from before runplane opened pull requests must get write access there first. */
  permissionsUrl: string | undefined
}

/**
 * "Open a pull request": runplane makes the change of `workflowDiff` in each
 * workflow of a repository and opens a pull request. Its jobs then come to
 * runplane, and JobWait sees the first one.
 */
export function PullRequestForm({ repositories, label, permissionsUrl }: PullRequestFormProps) {
  const [repository, setRepository] = useState<string | null>(repositories.length === 1 ? (repositories[0] ?? null) : null)
  const [unselected, setUnselected] = useState(false)
  const create = $api.useMutation('post', '/api/github/pull-requests')
  const headingId = useId()

  function submit(event: FormEvent) {
    event.preventDefault()
    if (repository) create.mutate({ body: { repository, label } })
    else setUnselected(true)
  }

  return (
    <section aria-labelledby={headingId} className="flex flex-col gap-xs">
      <h2 id={headingId} className="text-s font-medium text-neutral-10">
        Open a pull request
      </h2>
      {create.data ? (
        <div className="flex flex-wrap items-center justify-between gap-m rounded-m bg-neutral-2 p-m">
          <p role="status" className="flex min-w-0 items-center gap-s text-s font-medium text-neutral-10">
            <StatusIcon status="succeeded" />
            <span className="min-w-0">
              Pull request #{create.data.number} is open in {repository}
            </span>
          </p>
          <a href={create.data.html_url} target="_blank" rel="noreferrer" className={buttonClassName('secondary')}>
            View on GitHub
            <ExternalLink aria-hidden />
            <span className="sr-only"> (opens in a new tab)</span>
          </a>
        </div>
      ) : (
        <form onSubmit={submit} className="flex flex-col gap-s rounded-m bg-neutral-2 p-m">
          <div className="flex flex-wrap items-center gap-s">
            <Combobox
              items={repositories}
              value={repository}
              onValueChange={(name: string | null) => {
                setRepository(name)
                setUnselected(false)
                create.reset()
              }}
              autoHighlight
              disabled={create.isPending}
            >
              <ComboboxInput
                aria-label="Repository"
                placeholder="Search repositories"
                autoComplete="off"
                spellCheck={false}
                className="w-auto min-w-[240px] flex-1"
              />
              <ComboboxPopup empty="No repository matches.">
                {(name: string) => (
                  <ComboboxItem key={name} value={name}>
                    {name}
                  </ComboboxItem>
                )}
              </ComboboxPopup>
            </Combobox>
            <Button type="submit" variant="primary" loading={create.isPending}>
              Open pull request
            </Button>
          </div>
          <p className="text-s text-neutral-7">
            runplane makes this change for each job on ubuntu-latest or ubuntu-24.04 in the repository, then opens a pull request for you to review.
          </p>
          {unselected && <FormAlert>Select a repository in the list.</FormAlert>}
          {create.error && (
            <FormAlert>
              {errorMessage(create.error)}
              {isApiError(create.error, 403) && permissionsUrl && (
                <>
                  {' '}
                  <ExternalTextLink href={permissionsUrl}>
                    App permissions
                    <ExternalLink size={14} aria-hidden />
                    <span className="sr-only"> (opens in a new tab)</span>
                  </ExternalTextLink>
                </>
              )}
            </FormAlert>
          )}
        </form>
      )}
    </section>
  )
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

/** The waiting row. `onJob` runs when runplane has a job, or reports the state of one. */
export function JobWait({ onJob }: { onJob: () => void }) {
  useJobEvents(onJob)
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

const jobState: LiveEvent = 'job.state'

// The setup pages have no app layout, so no shared event stream; this page
// opens its own. runplane sees only jobs that ask for one of its labels, so
// any job event means the first job has arrived. The stream sends no past
// events, so each time it opens, has_jobs of the status tells whether a job
// came before.
function useJobEvents(onJob: () => void) {
  const queryClient = useQueryClient()
  const handle = useEffectEvent(onJob)
  const shows = usePageShows()

  useEffect(() => {
    const source = new EventSource('/api/events')
    source.onopen = () => {
      queryClient.fetchQuery({ ...statusQuery, staleTime: 0 }).then(
        (status) => status.has_jobs && handle(),
        () => {}, // The stream still waits for a job.
      )
    }
    source.addEventListener(jobState, () => handle())
    // A page in the back/forward cache keeps its stream open otherwise; `shows` opens it again.
    const close = () => source.close()
    addEventListener('pagehide', close)
    return () => {
      removeEventListener('pagehide', close)
      close()
    }
  }, [queryClient, shows])
}
