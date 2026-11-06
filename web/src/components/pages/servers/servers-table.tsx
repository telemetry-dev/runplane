import {
  CellTitle,
  DataTable,
  DataTableCell,
  DataTableEmpty,
  DataTableHead,
  DataTableHeader,
  DataTableIconHead,
  DataTableRow,
  RowLink,
  useFittingColumns,
} from '~/components/app/data-table'
import { None } from '~/components/app/none'
import { serverStatusNames, StatusDot } from '~/components/app/status-dot'
import { UsageBar } from '~/components/app/usage-bar'
import { Spinner } from '~/components/ui/spinner'
import { ServerMenu } from './server-menu'
import {
  agentUpdateNote,
  currentUsage,
  jobCount,
  serverHardware,
  serverOrigin,
  serverSize,
  sortServers,
  statusFacts,
  statusNote,
  type Server,
} from './server-info'

type ServersTableProps = {
  /** Undefined while the list loads. */
  servers: readonly Server[] | undefined
  /** Shows the row menus; their actions need the admin role. */
  admin: boolean
}

// A narrow table drops Resources, then Memory, then CPU.
const columns = {
  status: 16,
  server: 'grow',
  resources: { width: 170, drop: 1 },
  cpu: { width: 140, drop: 3 },
  memory: { width: 140, drop: 2 },
  running: 64,
  menu: 24,
} as const

type Shown = Record<keyof typeof columns, boolean>

/** The table of the Servers design. */
export function ServersTable({ servers, admin }: ServersTableProps) {
  const { ref, shown, columns: widths } = useFittingColumns(columns)

  return (
    <div ref={ref}>
      <DataTable aria-label="Servers" columns={widths} rowSpacing="m">
        <DataTableHeader>
          <DataTableIconHead label="Status" />
          <DataTableHead>Server</DataTableHead>
          {shown.resources && <DataTableHead>Resources</DataTableHead>}
          {shown.cpu && <DataTableHead>CPU</DataTableHead>}
          {shown.memory && <DataTableHead>Memory</DataTableHead>}
          <DataTableHead align="end">Running</DataTableHead>
          <DataTableHead>
            <span className="sr-only">Actions</span>
          </DataTableHead>
        </DataTableHeader>
        {!servers ? (
          <DataTableEmpty>
            <span role="status" className="inline-flex items-center gap-s">
              <Spinner />
              Loading servers
            </span>
          </DataTableEmpty>
        ) : servers.length === 0 ? (
          <DataTableEmpty>No servers yet.</DataTableEmpty>
        ) : (
          sortServers(servers).map((server) => <ServerRow key={server.id} server={server} shown={shown} admin={admin} />)
        )}
      </DataTable>
    </div>
  )
}

function noteText(note: { title: string; detail: string }): string {
  return [note.title, note.detail].filter(Boolean).join(' · ')
}

// The root of a usage bar also holds the gap and the 4ch percent.
const bar = 'w-[calc(88px+var(--space-s)+4ch)] text-xs'

function ServerRow({ server, shown, admin }: { server: Server; shown: Shown; admin: boolean }) {
  const size = serverSize(server)
  const note = statusNote(server)
  // Installing and unreachable servers run no jobs: their note spans Running
  // and the usage columns. Running alone has no room for it, so the note then
  // takes the place of the origin, and the name spans Running (a narrow table
  // has dropped Resources before the usage columns).
  const usage = Number(shown.cpu) + Number(shown.memory)
  const noteBelow = note && usage === 0
  return (
    <DataTableRow>
      <DataTableCell>
        <StatusDot status={server.status} />
      </DataTableCell>
      <DataTableCell className={noteBelow ? 'col-span-2' : undefined}>
        <RowLink to="/servers/$serverId" params={{ serverId: server.id }}>
          <CellTitle
            title={<NameWithUpdate server={server} />}
            detail={noteBelow ? noteText(note) : serverOrigin(server)}
            muted={statusFacts[server.status].muted}
          />
        </RowLink>
      </DataTableCell>
      {shown.resources && (
        <DataTableCell>
          {size ? <CellTitle title={<span className="font-regular text-neutral-8">{size}</span>} detail={serverHardware(server)} /> : <None />}
        </DataTableCell>
      )}
      {noteBelow ? null : note ? (
        <DataTableCell className={usage === 2 ? 'col-span-3' : 'col-span-2'}>
          <span className="shrink-0 font-medium text-neutral-10">{note.title}</span>
          {note.detail && <span className="truncate text-neutral-7">· {note.detail}</span>}
        </DataTableCell>
      ) : (
        <ServerUsage server={server} shown={shown} />
      )}
      <DataTableCell align="end" className="relative">
        {admin && <ServerMenu server={server} />}
      </DataTableCell>
    </DataTableRow>
  )
}

/** The name, and after it the state of an agent update, as "You" follows a name in Members. */
function NameWithUpdate({ server }: { server: Server }) {
  const update = agentUpdateNote(server)
  if (!update) return server.name
  return (
    <span className="flex items-baseline gap-xs">
      <span className="truncate">{server.name}</span>
      <span className="shrink-0 text-xs font-regular text-neutral-7">{update}</span>
    </span>
  )
}

function ServerUsage({ server, shown }: { server: Server; shown: Shown }) {
  const { cpu, memory } = currentUsage(server)
  const paused = statusFacts[server.status].takesRunners ? undefined : serverStatusNames[server.status]
  return (
    <>
      {shown.cpu && (
        <DataTableCell>
          <Usage value={cpu} label="CPU" />
        </DataTableCell>
      )}
      {shown.memory && (
        <DataTableCell>
          <Usage value={memory} label="Memory" />
        </DataTableCell>
      )}
      <DataTableCell align="end">
        <div className="flex flex-col items-end">
          <span className={server.running_jobs === 0 ? 'text-neutral-6' : undefined}>{jobCount(server.running_jobs)}</span>
          {paused && <span className="text-xs text-neutral-7">{paused}</span>}
        </div>
      </DataTableCell>
    </>
  )
}

function Usage({ value, label }: { value: number | null; label: string }) {
  if (value === null) return <None />
  return <UsageBar value={value} label={label} showValue className={bar} />
}
