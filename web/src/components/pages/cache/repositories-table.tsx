import { useQueryClient } from '@tanstack/react-query'
import { Link } from '@tanstack/react-router'
import { Ellipsis, Trash2 } from 'lucide-react'
import { useState } from 'react'
import { cachesQuery } from '~/api/caches'
import { api } from '~/api/client'
import { ConfirmDialog } from '~/components/app/confirm-dialog'
import {
  DataTable,
  DataTableCell,
  DataTableEmpty,
  DataTableHead,
  DataTableHeader,
  DataTableRow,
  useFittingColumns,
  type DataTableColumn,
} from '~/components/app/data-table'
import { None } from '~/components/app/none'
import { SkeletonRows } from '~/components/app/skeleton-rows'
import { UsageBar } from '~/components/app/usage-bar'
import { IconButton } from '~/components/ui/button'
import { Menu, MenuItem, MenuPopup, MenuTrigger } from '~/components/ui/menu'
import { toastManager } from '~/components/ui/toast'
import { formatBytes, formatCount, formatTimeAgo } from '~/lib/format'
import { useNow } from '~/lib/use-now'
import { repositoryHitRate, type CacheOverview, type CacheRepository } from './cache-format'

type RepositoriesTableProps = {
  /** Undefined while the overview loads. */
  cache: CacheOverview | undefined
  /** Shows the row menus; removing entries needs the admin role. */
  admin: boolean
}

/**
 * The cache use of each repository, largest first as the API sorts them. A
 * narrow table drops Last used, then Entries, the size bar, and Hit rate.
 */
export function RepositoriesTable({ cache, admin }: RepositoriesTableProps) {
  const now = useNow(60_000)
  const fit = useFittingColumns({
    repository: 'grow',
    // The bar sits in the Size column: with the gap after it, it makes that column 220px.
    bar: { width: 112, drop: 3 },
    size: 96,
    entries: { width: 80, drop: 2 },
    hitRate: { width: 80, drop: 4 },
    lastUsed: { width: 88, drop: 1 },
    menu: 24,
  })
  const shown = fit.shown
  const columns: DataTableColumn[] = shown.bar ? ['grow', 220, ...fit.columns.slice(3)] : fit.columns

  return (
    <div ref={fit.ref}>
      <DataTable aria-label="Cache by repository" columns={columns} rowSpacing="m">
        <DataTableHeader>
          <DataTableHead>Repository</DataTableHead>
          <DataTableHead>Size</DataTableHead>
          {shown.entries && <DataTableHead align="end">Entries</DataTableHead>}
          {shown.hitRate && <DataTableHead align="end">Hit rate</DataTableHead>}
          {shown.lastUsed && <DataTableHead align="end">Last used</DataTableHead>}
          <DataTableHead>
            <span className="sr-only">Actions</span>
          </DataTableHead>
        </DataTableHeader>
        {!cache ? (
          <SkeletonRows label="Loading the cache" />
        ) : !cache.enabled ? (
          <DataTableEmpty>
            The Actions cache is off. Turn it on in{' '}
            <Link to="/settings/runners" className="font-medium text-neutral-10 underline underline-offset-2">
              Settings › Runners
            </Link>
            .
          </DataTableEmpty>
        ) : cache.repositories.length === 0 ? (
          <DataTableEmpty>No cache entries yet. Jobs that use actions/cache save their entries here.</DataTableEmpty>
        ) : (
          cache.repositories.map((repo) => (
            <RepositoryRow key={repo.repository} repo={repo} maxBytes={cache.max_bytes} shown={shown} admin={admin} now={now} />
          ))
        )}
      </DataTable>
    </div>
  )
}

type RepositoryRowProps = {
  repo: CacheRepository
  maxBytes: number
  shown: Record<'bar' | 'entries' | 'hitRate' | 'lastUsed', boolean>
  admin: boolean
  now: number
}

function RepositoryRow({ repo, maxBytes, shown, admin, now }: RepositoryRowProps) {
  const hitRate = repositoryHitRate(repo)
  return (
    <DataTableRow>
      <DataTableCell>
        <span className="truncate font-medium text-neutral-10">{repo.repository}</span>
      </DataTableCell>
      <DataTableCell>
        <div className="flex min-w-0 items-center gap-s">
          {shown.bar && (
            <UsageBar
              value={maxBytes > 0 ? (repo.size_bytes / maxBytes) * 100 : 0}
              label="Share of the cache limit"
              className="w-[140px] shrink-0"
            />
          )}
          <span className="truncate">{formatBytes(repo.size_bytes)}</span>
        </div>
      </DataTableCell>
      {shown.entries && <DataTableCell align="end">{formatCount(repo.entries)}</DataTableCell>}
      {shown.hitRate && <DataTableCell align="end">{hitRate ?? <None />}</DataTableCell>}
      {shown.lastUsed && (
        <DataTableCell align="end">
          {repo.last_used_at ? <span className="text-neutral-7">{formatTimeAgo(repo.last_used_at, now)}</span> : <None />}
        </DataTableCell>
      )}
      <DataTableCell align="end">{admin && repo.entries > 0 && <RepositoryMenu repo={repo} />}</DataTableCell>
    </DataTableRow>
  )
}

function RepositoryMenu({ repo }: { repo: CacheRepository }) {
  const [removing, setRemoving] = useState(false)
  return (
    <>
      <Menu>
        <MenuTrigger render={<IconButton label={`${repo.repository} actions`} className="-m-xs" />}>
          <Ellipsis aria-hidden />
        </MenuTrigger>
        <MenuPopup align="end">
          <MenuItem icon={<Trash2 />} danger onClick={() => setRemoving(true)}>
            Remove all entries
          </MenuItem>
        </MenuPopup>
      </Menu>
      <RemoveEntriesDialog repo={repo} open={removing} onOpenChange={setRemoving} />
    </>
  )
}

type RemoveEntriesDialogProps = { repo: CacheRepository; open: boolean; onOpenChange: (open: boolean) => void }

function RemoveEntriesDialog({ repo, open, onOpenChange }: RemoveEntriesDialogProps) {
  const client = useQueryClient()
  const entries = repo.entries === 1 ? 'the cache entry' : `all ${formatCount(repo.entries)} cache entries`
  return (
    <ConfirmDialog
      open={open}
      onOpenChange={onOpenChange}
      title={`Remove ${entries} of ${repo.repository}?`}
      description="The next jobs of this repository run without these caches and save new entries."
      action="Remove entries"
      onConfirm={async () => {
        const { data } = await api.DELETE('/api/caches', { params: { query: { repository: repo.repository } } })
        const deleted = data?.deleted ?? 0
        toastManager.add({ title: deleted === 1 ? 'Removed 1 entry' : `Removed ${formatCount(deleted)} entries` })
      }}
      onDone={() => void client.invalidateQueries({ queryKey: cachesQuery.queryKey })}
    />
  )
}
