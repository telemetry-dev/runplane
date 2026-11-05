import { useQuery, useQueryClient } from '@tanstack/react-query'
import { CirclePause, CirclePlay, Ellipsis, Pencil, Trash2 } from 'lucide-react'
import { useState, type ReactNode } from 'react'
import { $api, errorMessage, type Schemas } from '~/api/client'
import { dropPool, putPool, refreshRunners } from '~/api/pools'
import { serversQuery } from '~/api/servers'
import {
  CellTitle,
  DataTable,
  DataTableCell,
  DataTableEmpty,
  DataTableHead,
  DataTableHeader,
  DataTableRow,
  useFittingColumns,
} from '~/components/app/data-table'
import { AlertDialog, AlertDialogClose, AlertDialogFooter, AlertDialogPopup } from '~/components/ui/alert-dialog'
import { Button, IconButton } from '~/components/ui/button'
import { Menu, MenuItem, MenuPopup, MenuSeparator, MenuTrigger } from '~/components/ui/menu'
import { toastManager } from '~/components/ui/toast'
import { Code } from './code'
import { poolHardware, poolScope, poolSize, type Pool } from './runner-format'
import { SkeletonRows } from './skeleton-rows'

type LabelsTableProps = {
  /** Undefined while the list loads. */
  pools: readonly Pool[] | undefined
  /** Shows the row menus; their actions need the admin role. */
  admin: boolean
  /** The label that a link points to; its row has the selected fill. */
  marked?: string
  onEdit: (pool: Pool) => void
}

// A narrow table drops Image, then Size, then Warm / max and Starting, then Idle.
const columns = {
  label: 'grow',
  size: { width: 150, drop: 2 },
  image: { width: 140, drop: 1 },
  warm: { width: 80, drop: 3 },
  idle: { width: 48, drop: 4 },
  busy: 48,
  starting: { width: 60, drop: 3 },
  queued: 56,
  menu: 24,
} as const

type Shown = Record<keyof typeof columns, boolean>

/** The Labels table of the Runners design. */
export function LabelsTable({ pools, admin, marked, onEdit }: LabelsTableProps) {
  const servers = useQuery(serversQuery).data
  const { ref, shown, columns: widths } = useFittingColumns(columns)

  return (
    <div ref={ref}>
      <DataTable aria-label="Labels" columns={widths} rowSpacing="m">
        <DataTableHeader>
          <DataTableHead>Label</DataTableHead>
          {shown.size && <DataTableHead>Size</DataTableHead>}
          {shown.image && <DataTableHead>Image</DataTableHead>}
          {shown.warm && <DataTableHead align="end">Warm / max</DataTableHead>}
          {shown.idle && <DataTableHead align="end">Idle</DataTableHead>}
          <DataTableHead align="end">Busy</DataTableHead>
          {shown.starting && <DataTableHead align="end">Starting</DataTableHead>}
          <DataTableHead align="end">Queued</DataTableHead>
          <DataTableHead>
            <span className="sr-only">Actions</span>
          </DataTableHead>
        </DataTableHeader>
        {!pools ? (
          <SkeletonRows lines={2} label="Loading labels" />
        ) : pools.length === 0 ? (
          <DataTableEmpty>No labels. Add one to run jobs on your servers.</DataTableEmpty>
        ) : (
          pools.map((pool) => (
            <LabelRow
              key={pool.id}
              pool={pool}
              servers={servers}
              shown={shown}
              admin={admin}
              marked={pool.label === marked}
              onEdit={onEdit}
            />
          ))
        )}
      </DataTable>
    </div>
  )
}

type LabelRowProps = {
  pool: Pool
  servers: Schemas['Server'][] | undefined
  shown: Shown
  admin: boolean
  marked: boolean
  onEdit: (pool: Pool) => void
}

function LabelRow({ pool, servers, shown, admin, marked, onEdit }: LabelRowProps) {
  return (
    <DataTableRow selected={marked}>
      <DataTableCell>
        <CellTitle title={<Code>{pool.label}</Code>} detail={<PoolScope pool={pool} servers={servers} />} muted={!pool.enabled} />
      </DataTableCell>
      {shown.size && (
        <DataTableCell>
          <CellTitle title={<span className="font-regular text-neutral-8">{poolSize(pool)}</span>} detail={poolHardware(pool)} />
        </DataTableCell>
      )}
      {shown.image && (
        <DataTableCell>
          <span className="truncate">{pool.image_name}</span>
        </DataTableCell>
      )}
      {shown.warm && (
        <DataTableCell align="end">
          <span>
            {pool.min_idle}
            <span className="text-neutral-6"> / {pool.max_runners}</span>
          </span>
        </DataTableCell>
      )}
      {pool.enabled ? (
        <>
          {shown.idle && <Count value={pool.idle} />}
          <Count value={pool.busy} />
          {shown.starting && <Count value={pool.starting} />}
          <Count value={pool.queued} strong />
        </>
      ) : (
        // A disabled pool runs nothing, so its state takes the count lanes.
        <DataTableCell align="end" className={countSpans[Number(shown.idle) + Number(shown.starting)]}>
          <span className="font-medium text-neutral-10">Disabled</span>
        </DataTableCell>
      )}
      <DataTableCell align="end">{admin && <LabelMenu pool={pool} onEdit={onEdit} />}</DataTableCell>
    </DataTableRow>
  )
}

// Busy and Queued always show; Idle and Starting add a lane each.
const countSpans = ['col-span-2', 'col-span-3', 'col-span-4']

/** The servers of a pool, or why none can start its runners. */
function PoolScope({ pool, servers }: { pool: Pool; servers: Schemas['Server'][] | undefined }) {
  if (!pool.enabled || pool.placeable_servers > 0) return poolScope(pool, servers)
  return (
    <span className="text-neutral-10">
      {pool.online_servers === 0 ? `No ${pool.arch} server is online` : `No ${pool.arch} server is large enough`}
    </span>
  )
}

/** A live count; zero recedes. `strong` marks waiting jobs, which need attention. */
function Count({ value, strong = false }: { value: number; strong?: boolean }) {
  const tone = value === 0 ? 'text-neutral-6' : strong ? 'font-medium text-neutral-10' : undefined
  return (
    <DataTableCell align="end">
      <span className={tone}>{value}</span>
    </DataTableCell>
  )
}

function LabelMenu({ pool, onEdit }: { pool: Pool; onEdit: (pool: Pool) => void }) {
  const client = useQueryClient()
  const [deleting, setDeleting] = useState(false)
  const update = $api.useMutation('patch', '/api/pools/{id}', {
    onSuccess: (updated) => {
      putPool(client, updated)
      void refreshRunners(client)
    },
    onError: (error, { body }) =>
      toastManager.add({ title: `Could not ${body.enabled ? 'enable' : 'disable'} ${pool.label}`, description: errorMessage(error) }),
  })

  return (
    <>
      <Menu>
        <MenuTrigger render={<IconButton label={`${pool.label} actions`} className="-m-xs" />}>
          <Ellipsis aria-hidden />
        </MenuTrigger>
        <MenuPopup align="end">
          <MenuItem icon={<Pencil />} onClick={() => onEdit(pool)}>
            Edit
          </MenuItem>
          <MenuItem
            icon={pool.enabled ? <CirclePause /> : <CirclePlay />}
            onClick={() => update.mutate({ params: { path: { id: pool.id } }, body: { enabled: !pool.enabled } })}
          >
            {pool.enabled ? 'Disable' : 'Enable'}
          </MenuItem>
          <MenuSeparator />
          <MenuItem icon={<Trash2 />} danger onClick={() => setDeleting(true)}>
            Delete
          </MenuItem>
        </MenuPopup>
      </Menu>
      <DeleteDialog pool={pool} open={deleting} onOpenChange={setDeleting} />
    </>
  )
}

type DeleteDialogProps = { pool: Pool; open: boolean; onOpenChange: (open: boolean) => void }

function DeleteDialog({ pool, open, onOpenChange }: DeleteDialogProps) {
  const client = useQueryClient()
  const remove = $api.useMutation('delete', '/api/pools/{id}', {
    onSuccess: () => {
      onOpenChange(false)
      toastManager.add({ title: `Deleted ${pool.label}` })
    },
  })

  return (
    <AlertDialog
      open={open}
      onOpenChange={onOpenChange}
      onOpenChangeComplete={(isOpen) => {
        if (isOpen) return
        // Drop the row only after the close animation; the dialog lives in it.
        if (remove.isSuccess) {
          dropPool(client, pool.id)
          void refreshRunners(client)
        }
        remove.reset()
      }}
    >
      <AlertDialogPopup title={`Delete ${pool.label}?`} description={deleteDescription(pool)}>
        {remove.error && <ErrorText>{errorMessage(remove.error)}</ErrorText>}
        <AlertDialogFooter>
          <AlertDialogClose render={<Button />}>Cancel</AlertDialogClose>
          <Button variant="danger" loading={remove.isPending} onClick={() => remove.mutate({ params: { path: { id: pool.id } } })}>
            Delete label
          </Button>
        </AlertDialogFooter>
      </AlertDialogPopup>
    </AlertDialog>
  )
}

function deleteDescription(pool: Pool): string {
  const runners = pool.idle + pool.busy + pool.starting
  const stop = runners === 0 ? '' : runners === 1 ? ', and its runner stops' : `, and its ${runners} runners stop`
  return `Workflows that use ${pool.label} stop getting runners${stop}.`
}

function ErrorText({ children }: { children: ReactNode }) {
  return (
    <p role="alert" className="text-s text-error">
      {children}
    </p>
  )
}
