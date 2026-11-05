import { keepPreviousData, useQuery } from '@tanstack/react-query'
import { useState } from 'react'
import { auditQuery } from '~/api/audit'
import {
  DataTable,
  DataTableCell,
  DataTableEmpty,
  DataTableHead,
  DataTableHeader,
  DataTableRow,
  useFittingColumns,
} from '~/components/app/data-table'
import { None } from '~/components/app/none'
import { RunnerLabel } from '~/components/app/runner-label'
import { Avatar } from '~/components/ui/avatar'
import { Button } from '~/components/ui/button'
import { Skeleton } from '~/components/ui/skeleton'
import { Tooltip } from '~/components/ui/tooltip'
import { formatDateTime } from '~/lib/format'
import { auditActor, auditChange, auditTarget, auditTime, type AuditEntry } from './audit-format'
import { LoadError, SettingsSection } from './parts'

/** The Activity section of Settings · Members: the audit log in pages, newest first. For admins. */
export function Activity() {
  // The cursors of the pages so far; the last one is on screen.
  const [cursors, setCursors] = useState([''])
  const audit = useQuery({ ...auditQuery(cursors.at(-1)!), placeholderData: keepPreviousData })
  // A narrow table drops Target.
  const fit = useFittingColumns({ time: 96, change: 'grow', target: { width: 120, drop: 1 } })
  const entries = audit.data?.items
  const next = audit.data?.next_cursor

  return (
    <SettingsSection title="Activity" table>
      {audit.isError && !audit.data ? (
        <LoadError what="the activity" onRetry={() => void audit.refetch()} retrying={audit.isFetching} />
      ) : (
        <div ref={fit.ref} className="flex flex-col gap-s">
          <DataTable aria-label="Activity" columns={fit.columns}>
            <DataTableHeader>
              <DataTableHead>Time</DataTableHead>
              <DataTableHead>
                {/* Over the sentence, after the avatar. */}
                <span className="pl-[calc(24px+var(--space-s))]">Change</span>
              </DataTableHead>
              {fit.shown.target && <DataTableHead>Target</DataTableHead>}
            </DataTableHeader>
            {!entries ? (
              <SkeletonRows target={fit.shown.target} />
            ) : entries.length === 0 ? (
              <DataTableEmpty>No changes yet.</DataTableEmpty>
            ) : (
              entries.map((entry) => <AuditRow key={entry.id} entry={entry} target={fit.shown.target} />)
            )}
          </DataTable>
          {(cursors.length > 1 || next) && (
            <nav aria-label="Activity pages" className="flex justify-end gap-xs pt-xs">
              <Button disabled={cursors.length === 1} onClick={() => setCursors(cursors.slice(0, -1))}>
                Previous
              </Button>
              <Button
                disabled={!next}
                loading={audit.isPlaceholderData && audit.isFetching}
                onClick={() => next && setCursors([...cursors, next])}
              >
                Next
              </Button>
            </nav>
          )}
        </div>
      )}
    </SettingsSection>
  )
}

function AuditRow({ entry, target }: { entry: AuditEntry; target: boolean }) {
  const actor = auditActor(entry)
  const object = auditTarget(entry)
  return (
    <DataTableRow>
      <DataTableCell>
        <Tooltip content={formatDateTime(entry.at, { full: true })}>
          <time dateTime={entry.at} className="truncate">
            {auditTime(entry.at)}
          </time>
        </Tooltip>
      </DataTableCell>
      <DataTableCell className="gap-s">
        <Avatar name={actor} />
        <p className="min-w-0 flex-1">
          <span className="font-medium text-neutral-10">{actor}</span> {auditChange(entry)}
        </p>
      </DataTableCell>
      {target && (
        <DataTableCell>
          {!object ? (
            <None />
          ) : object.label ? (
            <RunnerLabel className="leading-s">{object.name}</RunnerLabel>
          ) : (
            <span className="truncate">{object.name}</span>
          )}
        </DataTableCell>
      )}
    </DataTableRow>
  )
}

function SkeletonRows({ target }: { target: boolean }) {
  return Array.from({ length: 3 }, (_, index) => (
    <DataTableRow key={index}>
      <DataTableCell>
        <Skeleton className="my-[3px] h-[12px] w-[64px]" />
      </DataTableCell>
      <DataTableCell className="gap-s">
        <Skeleton className="size-[24px] rounded-full" />
        <Skeleton className="my-[3px] h-[12px] w-[240px] max-w-full" />
      </DataTableCell>
      {target && (
        <DataTableCell>
          <Skeleton className="my-[3px] h-[12px] w-[80px]" />
        </DataTableCell>
      )}
    </DataTableRow>
  ))
}
