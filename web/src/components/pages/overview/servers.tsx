import type { Schemas } from '~/api/client'
import { useIsAdmin } from '~/api/session'
import { CellTitle, DataTable, DataTableCell, DataTableRow, RowLink, useFittingColumns } from '~/components/app/data-table'
import { SectionButton, SectionHeader, SectionLink } from '~/components/app/section-header'
import { serverStatusNames, StatusDot } from '~/components/app/status-dot'
import { UsageBar } from '~/components/app/usage-bar'
import { AddServerDialog } from '~/components/pages/servers/add-server-dialog'
import { jobCount } from '~/components/pages/servers/server-info'
import { formatMegabytes } from '~/lib/format'

type Server = Schemas['ServerSummary']

/** The "Servers" card beside the chart: state, size, live CPU, and running jobs of each server. */
export function ServersCard({ servers }: { servers: readonly Server[] }) {
  return (
    <section className="flex min-w-0 flex-col gap-m rounded-m bg-neutral-2 p-l xl:grow-2 xl:basis-0">
      <SectionHeader title="Servers" action={<SectionLink to="/servers">Manage</SectionLink>} />
      <ul className="flex flex-col">
        {servers.map((server) => (
          // The divider gets its own pixel of padding, as a border would.
          <li
            key={server.id}
            className="relative flex items-center gap-m py-s [&:not(:last-child)]:pb-[calc(var(--space-s)+1px)] [&:not(:last-child)]:shadow-[inset_0_-1px_0_var(--neutral-4)]"
          >
            {/* The card shows the 8px dot without the 16px slot of the tables. */}
            <StatusDot status={server.status} className="-mx-[4px]" />
            <ServerName server={server} detail={specs(server)} />
            <div className="w-[132px] shrink-0">
              <ServerUsage server={server} />
            </div>
            <span className="w-[44px] shrink-0 text-right text-xs text-neutral-8">{jobs(server)}</span>
          </li>
        ))}
      </ul>
    </section>
  )
}

/** The servers under "No jobs yet", where the next step may be to add one. */
export function ServerList({ servers }: { servers: readonly Server[] }) {
  const admin = useIsAdmin()
  const fit = useFittingColumns({ status: 16, server: 'grow', cpu: { width: 132, drop: 1 }, jobs: 56 })
  return (
    <section className="flex flex-col gap-s">
      <SectionHeader title="Servers" action={admin && <AddServerDialog trigger={<SectionButton>Add server</SectionButton>} />} />
      <div ref={fit.ref}>
        <DataTable aria-label="Servers" columns={fit.columns}>
          {servers.map((server) => (
            <DataTableRow key={server.id}>
              <DataTableCell>
                <StatusDot status={server.status} />
              </DataTableCell>
              <DataTableCell>
                <ServerName server={server} detail={server.kind === 'local' ? `This server · ${specs(server)}` : specs(server)} />
              </DataTableCell>
              {fit.shown.cpu && (
                <DataTableCell>
                  <ServerUsage server={server} />
                </DataTableCell>
              )}
              <DataTableCell align="end" className="text-xs">
                {jobs(server)}
              </DataTableCell>
            </DataTableRow>
          ))}
        </DataTable>
      </div>
    </section>
  )
}

function ServerName({ server, detail }: { server: Server; detail: string }) {
  return (
    <RowLink to="/servers/$serverId" params={{ serverId: server.id }} className="flex-1">
      <CellTitle title={server.name} detail={detail} muted={server.status === 'unreachable' || server.status === 'disabled'} />
    </RowLink>
  )
}

/** CPU of a server that reports it; its state otherwise. */
function ServerUsage({ server }: { server: Server }) {
  const cpu = server.cpu_percent
  // A 92px bar, as in the designs. The root also holds the gap and the 4ch percent.
  if (reporting(server) && cpu !== null) {
    return <UsageBar value={cpu} label={`${server.name} CPU`} showValue className="w-[calc(92px+var(--space-s)+4ch)] text-xs" />
  }
  return <span className="block min-w-0 truncate text-xs text-neutral-7">{reporting(server) ? 'No data yet' : serverStatusNames[server.status]}</span>
}

/** Online and draining servers run jobs and send stats. */
function reporting(server: Server): boolean {
  return server.status === 'online' || server.status === 'draining'
}

function specs(server: Server): string {
  const parts = [server.vcpu > 0 && `${server.vcpu} vCPU`, server.memory_mb > 0 && formatMegabytes(server.memory_mb), server.arch]
  return parts.filter(Boolean).join(' · ')
}

function jobs(server: Server): string {
  return reporting(server) ? jobCount(server.running_jobs) : ''
}
