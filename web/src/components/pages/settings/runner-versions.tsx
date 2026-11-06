import { useQuery } from '@tanstack/react-query'
import { TriangleAlert } from 'lucide-react'
import { runnerVersionsQuery } from '~/api/runner-versions'
import {
  DataTable,
  DataTableCell,
  DataTableEmpty,
  DataTableHead,
  DataTableHeader,
  DataTableIconHead,
  DataTableRow,
  useFittingColumns,
} from '~/components/app/data-table'
import { ErrorHint } from '~/components/app/error-hint'
import { LoadErrorRow } from '~/components/app/load-error'
import { None } from '~/components/app/none'
import { SkeletonCell, SkeletonRows } from '~/components/app/skeleton-rows'
import { StatusIcon } from '~/components/app/status-icon'
import { formatDate } from '~/lib/format'
import { Badge, SettingsSection } from './parts'
import { groupVersions } from './runner-info'

/** The GitHub runner versions that runplane has downloaded, newest first. */
export function RunnerVersions() {
  const versions = useQuery(runnerVersionsQuery)
  const groups = versions.data && groupVersions(versions.data.items, versions.data.current)
  // A narrow table drops Released, then Architectures, then the end date.
  const { ref, shown, columns } = useFittingColumns({
    status: 16,
    version: 'grow',
    architectures: { width: 110, drop: 2 },
    released: { width: 110, drop: 1 },
    endDate: { width: 150, drop: 3 },
  })

  return (
    <SettingsSection title="Runner version" table>
      <div ref={ref}>
        <DataTable aria-label="Runner versions" columns={columns}>
          <DataTableHeader>
            <DataTableIconHead label="Status" />
            <DataTableHead>Version</DataTableHead>
            {shown.architectures && <DataTableHead>Architectures</DataTableHead>}
            {shown.released && <DataTableHead>Released</DataTableHead>}
            {shown.endDate && <DataTableHead>GitHub end date</DataTableHead>}
          </DataTableHeader>
          {groups ? (
            groups.map((group) => (
              <DataTableRow key={group.version}>
                <DataTableCell>
                  <ErrorHint error={group.error}>
                    <StatusIcon status={group.status} />
                  </ErrorHint>
                </DataTableCell>
                <DataTableCell>
                  <span className="truncate font-medium text-neutral-10 tabular-nums">{group.version}</span>
                  {group.inUse && <Badge>{group.inUse}</Badge>}
                </DataTableCell>
                {shown.architectures && <DataTableCell>{group.architectures}</DataTableCell>}
                {shown.released && <DataTableCell>{group.released_at ? formatDate(group.released_at) : <None />}</DataTableCell>}
                {shown.endDate && (
                  <DataTableCell>
                    {!group.end_date ? (
                      <None />
                    ) : group.ends_soon ? (
                      <span className="flex items-center gap-xs font-medium text-neutral-10">
                        <TriangleAlert size={14} className="shrink-0" aria-hidden />
                        {formatDate(group.end_date)}
                        <span className="sr-only"> (soon)</span>
                      </span>
                    ) : (
                      formatDate(group.end_date)
                    )}
                  </DataTableCell>
                )}
              </DataTableRow>
            ))
          ) : versions.isError ? (
            <LoadErrorRow what="the runner versions" error={versions.error} onRetry={() => void versions.refetch()} retrying={versions.isFetching} />
          ) : (
            <SkeletonRows label="Loading the runner versions" count={2}>
              <SkeletonCell shape="icon" />
              <SkeletonCell lines={[64]} />
              {shown.architectures && <SkeletonCell lines={[72]} />}
              {shown.released && <SkeletonCell lines={[80]} />}
              {shown.endDate && <SkeletonCell lines={[24]} />}
            </SkeletonRows>
          )}
          {groups?.length === 0 && <DataTableEmpty>runplane has not checked for runner releases yet.</DataTableEmpty>}
        </DataTable>
      </div>
    </SettingsSection>
  )
}
