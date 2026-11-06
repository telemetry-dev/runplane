import { keepPreviousData, useQuery, type QueryClient } from '@tanstack/react-query'
import { useLocation, useNavigate, type HistoryState } from '@tanstack/react-router'
import { useCallback } from 'react'
import { facetsQuery, jobsQuery } from '~/api/jobs'
import { statusQuery } from '~/api/session'
import { LoadError } from '~/components/app/load-error'
import { Button } from '~/components/ui/button'
import { PageHeader } from '~/components/ui/page-header'
import { defaultRange, hasFilters, jobsApiQuery, pageRange, pageSize, rangeOptions, type JobsSearch } from './jobs-search'
import { JobsTable } from './jobs-table'
import { JobsToolbar } from './jobs-toolbar'

/** Starts the requests of the page, also on link hover; the page shows its own loading state. */
export function prefetch(client: QueryClient, search: JobsSearch) {
  void client.prefetchQuery(jobsQuery(jobsApiQuery(search)))
  void client.prefetchQuery(facetsQuery)
}

/** The "Jobs" design: search and filters, the jobs table, and cursor pages. */
export function JobsPage({ search }: { search: JobsSearch }) {
  const navigate = useNavigate()
  const before = useLocation({ select: (location) => previousCursors(location.state) })
  const jobs = useQuery({ ...jobsQuery(jobsApiQuery(search)), placeholderData: keepPreviousData })

  const change = useCallback(
    (next: Partial<JobsSearch>, typing = false) => {
      void navigate({ to: '/jobs', search: { ...search, ...next, cursor: undefined }, replace: typing })
    },
    [navigate, search],
  )

  const items = jobs.data?.items ?? null
  const nextCursor = jobs.data?.next_cursor
  // A link to a later page has no history of the pages before it, so its position is unknown.
  const offset = search.cursor && before.length === 0 ? null : before.length * pageSize

  function goTo(cursor: string | undefined, pages: string[]) {
    const state: HistoryState & { jobPages: string[] } = { jobPages: pages }
    void navigate({ to: '/jobs', search: { ...search, cursor }, state })
  }

  return (
    <>
      <PageHeader title="Jobs" />
      <div className="flex flex-col gap-m">
        <JobsToolbar search={search} onChange={change} />
        {jobs.isError && !jobs.data ? (
          <LoadError what="the jobs" error={jobs.error} onRetry={() => void jobs.refetch()} retrying={jobs.isFetching} />
        ) : (
          <JobsTable
            jobs={items}
            stale={jobs.isPlaceholderData}
            empty={<EmptyText search={search} onClear={() => change({ q: undefined, status: undefined, repository: undefined, pool: undefined, server: undefined })} />}
          />
        )}
        <p className="sr-only" aria-live="polite">
          {jobs.isFetching && !items ? 'Loading jobs' : ''}
        </p>
        {items && items.length > 0 && (
          <footer className="flex flex-wrap items-center justify-between gap-m">
            <p className="text-s text-neutral-7 tabular-nums">{pageRange(items.length, offset, jobs.data?.total ?? items.length)}</p>
            {(search.cursor || nextCursor) && (
              <nav aria-label="Pages" className="flex gap-xs">
                <Button disabled={!search.cursor} onClick={() => goTo(before.at(-1) || undefined, before.slice(0, -1))}>
                  Previous
                </Button>
                <Button disabled={!nextCursor} onClick={() => goTo(nextCursor, [...before, search.cursor ?? ''])}>
                  Next
                </Button>
              </nav>
            )}
          </footer>
        )}
      </div>
    </>
  )
}

function EmptyText({ search, onClear }: { search: JobsSearch; onClear: () => void }) {
  const hasJobs = useQuery(statusQuery).data?.has_jobs ?? true
  if (hasFilters(search)) {
    return (
      <span className="inline-flex flex-wrap items-center justify-center gap-s">
        No jobs match these filters.
        <Button variant="ghost" onClick={onClear}>
          Clear filters
        </Button>
      </span>
    )
  }
  if (!hasJobs || search.range === 'all') return 'No jobs yet. Jobs show here when a workflow runs on a runplane runner.'
  const range = rangeOptions.find((option) => option.value === (search.range ?? defaultRange))?.label.toLowerCase()
  return `No jobs in the ${range}.`
}

/**
 * Cursors of the pages before this one. The history entry keeps them, so
 * Previous and the "51–100" text work after Back, Forward, and a reload.
 * The first page has the cursor "".
 */
function previousCursors(state: object): string[] {
  if (!('jobPages' in state) || !Array.isArray(state.jobPages)) return []
  return state.jobPages.filter((cursor): cursor is string => typeof cursor === 'string')
}
