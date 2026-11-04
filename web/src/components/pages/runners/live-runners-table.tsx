import { Link } from '@tanstack/react-router'
import { Ellipsis, ScrollText, Square } from 'lucide-react'
import { useState, type ReactNode } from 'react'
import {
  CellTitle,
  DataTable,
  DataTableCell,
  DataTableEmpty,
  DataTableHead,
  DataTableHeader,
  DataTableIconHead,
  DataTableRow,
  useFittingColumns,
} from '~/components/app/data-table'
import { RunnerLabel } from '~/components/app/runner-label'
import { RunnerStateIcon } from '~/components/app/runner-state-icon'
import { workflowFile } from '~/components/pages/jobs/job-format'
import { useNow } from '~/components/pages/jobs/use-now'
import { IconButton } from '~/components/ui/button'
import { Menu, MenuItem, MenuPopup, MenuTrigger } from '~/components/ui/menu'
import { Code } from './code'
import { formatUptime, sortSandboxes, stateNames, type Sandbox } from './runner-format'
import { RunnerLogDrawer } from './runner-log'
import { SkeletonRows } from './skeleton-rows'
import { StopRunnerDialog } from './stop-runner-dialog'

type LiveRunnersTableProps = {
  /** Undefined while the list loads. */
  sandboxes: readonly Sandbox[] | undefined
  /** Server detail lists the runners of one server, so it drops the Server column. */
  showServer?: boolean
  /** The row of an empty list. */
  empty: ReactNode
  /** The row while the list did not load, in place of the loading rows. */
  failed?: ReactNode
}

/**
 * The Live runners table of the Runners design and of Server detail.
 * sandbox.state events refetch the list; the uptimes tick each second. A narrow
 * table drops Label (the runner name starts with it), then Server, then Job,
 * whose name then follows the state under the runner.
 */
export function LiveRunnersTable({ sandboxes, showServer = true, empty, failed }: LiveRunnersTableProps) {
  const now = useNow(1000)
  // The log drawer and the stop dialog keep their runner outside the rows: the
  // log stays open after the runner leaves the list, and the dialog closes with
  // its animation.
  const [log, setLog] = useState<Action>({ open: false })
  const [stop, setStop] = useState<Action>({ open: false })
  const listedLog = sandboxes?.find((sandbox) => sandbox.id === log.sandbox?.id)
  const listedStop = sandboxes?.find((sandbox) => sandbox.id === stop.sandbox?.id)
  const { ref, shown, columns } = useFittingColumns({
    state: 16,
    runner: 'grow',
    label: { width: 160, drop: 1 },
    server: showServer ? { width: 140, drop: 2 } : undefined,
    job: { width: 300, drop: 3 },
    uptime: 72,
    menu: 24,
  })

  return (
    <div ref={ref}>
      <DataTable aria-label={showServer ? 'Live runners' : 'Runners on this server'} columns={columns}>
        <DataTableHeader>
          <DataTableIconHead label="State" />
          <DataTableHead>Runner</DataTableHead>
          {shown.label && <DataTableHead>Label</DataTableHead>}
          {shown.server && <DataTableHead>Server</DataTableHead>}
          {shown.job && <DataTableHead>Job</DataTableHead>}
          <DataTableHead align="end">Uptime</DataTableHead>
          <DataTableHead>
            <span className="sr-only">Actions</span>
          </DataTableHead>
        </DataTableHeader>
        {!sandboxes ? (
          failed ? (
            <DataTableEmpty>{failed}</DataTableEmpty>
          ) : (
            <SkeletonRows lines={2} label="Loading runners" />
          )
        ) : sandboxes.length === 0 ? (
          <DataTableEmpty>{empty}</DataTableEmpty>
        ) : (
          sortSandboxes(sandboxes).map((sandbox) => (
            <RunnerRow
              key={sandbox.id}
              sandbox={sandbox}
              shown={shown}
              now={now}
              onViewLog={() => setLog({ open: true, sandbox })}
              onStop={() => setStop({ open: true, sandbox })}
            />
          ))
        )}
      </DataTable>
      {log.sandbox && (
        <RunnerLogDrawer
          sandbox={listedLog ?? log.sandbox}
          stopped={!listedLog}
          open={log.open}
          onOpenChange={(open) => setLog((current) => ({ ...current, open }))}
        />
      )}
      {stop.sandbox && (
        <StopRunnerDialog
          sandbox={listedStop ?? stop.sandbox}
          // A runner that left the list has stopped already.
          open={stop.open && listedStop !== undefined}
          onOpenChange={(open) => setStop((current) => ({ ...current, open }))}
        />
      )}
    </div>
  )
}

/** A drawer or dialog and the runner it shows. */
type Action = { open: boolean; sandbox?: Sandbox }

type RunnerRowProps = {
  sandbox: Sandbox
  shown: Record<'label' | 'server' | 'job', boolean>
  now: number
} & RunnerMenuProps

function RunnerRow({ sandbox, shown, now, onViewLog, onStop }: RunnerRowProps) {
  const state = stateNames[sandbox.state]
  return (
    <DataTableRow>
      <DataTableCell>
        <RunnerStateIcon state={sandbox.state} />
      </DataTableCell>
      <DataTableCell>
        <CellTitle
          title={<Code>{sandbox.runner_name}</Code>}
          detail={!shown.job && sandbox.job_name ? `${state} · ${sandbox.job_name}` : state}
        />
      </DataTableCell>
      {shown.label && (
        <DataTableCell>{sandbox.pool_label ? <RunnerLabel>{sandbox.pool_label}</RunnerLabel> : <None />}</DataTableCell>
      )}
      {shown.server && (
        <DataTableCell>
          <Link to="/servers/$serverId" params={{ serverId: sandbox.server_id }} className={inlineLink}>
            {sandbox.server_name}
          </Link>
        </DataTableCell>
      )}
      {shown.job && (
        <DataTableCell>
          {sandbox.job_id ? (
            <CellTitle
              title={
                <Link to="/jobs/$jobId" params={{ jobId: sandbox.job_id }} className={inlineLink}>
                  {sandbox.job_name || 'Job'}
                </Link>
              }
              detail={[sandbox.job_repository, workflowFile(sandbox.job_workflow_file)].filter(Boolean).join(' · ')}
            />
          ) : (
            <None />
          )}
        </DataTableCell>
      )}
      <DataTableCell align="end">
        <span className="tabular-nums">{formatUptime(now - Date.parse(sandbox.created_at))}</span>
      </DataTableCell>
      <DataTableCell align="end">
        <RunnerMenu sandbox={sandbox} onViewLog={onViewLog} onStop={onStop} />
      </DataTableCell>
    </DataTableRow>
  )
}

type RunnerMenuProps = {
  onViewLog: () => void
  onStop: () => void
}

/** View log and Stop runner. Members may do both. */
function RunnerMenu({ sandbox, onViewLog, onStop }: { sandbox: Sandbox } & RunnerMenuProps) {
  return (
    <Menu>
      <MenuTrigger render={<IconButton label={`${sandbox.runner_name} actions`} className="-m-xs" />}>
        <Ellipsis aria-hidden />
      </MenuTrigger>
      <MenuPopup align="end">
        <MenuItem icon={<ScrollText />} onClick={onViewLog}>
          View log
        </MenuItem>
        <MenuItem icon={<Square />} danger disabled={sandbox.state === 'stopping'} onClick={onStop}>
          Stop runner
        </MenuItem>
      </MenuPopup>
    </Menu>
  )
}

function None() {
  return <span className="text-neutral-6">—</span>
}

const inlineLink = 'truncate rounded-xs underline-offset-2 hover:underline'
