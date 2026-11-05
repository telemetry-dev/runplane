import { useQuery } from '@tanstack/react-query'
import type { ReactNode } from 'react'
import { errorMessage } from '~/api/client'
import { useFollowedText } from '~/api/followed-text'
import { followSandboxLog, sandboxQuery } from '~/api/sandboxes'
import { LogViewer } from '~/components/app/log-viewer'
import { sandboxStateNames } from '~/components/app/runner-state-icon'
import { TextButton } from '~/components/app/text-button'
import { Drawer, DrawerPopup } from '~/components/ui/drawer'
import type { Sandbox } from './runner-format'

type RunnerLogDrawerProps = {
  sandbox: Sandbox
  /** The runner left the live list, so `sandbox` holds its last listed state. */
  stopped: boolean
  open: boolean
  onOpenChange: (open: boolean) => void
}

/**
 * A side panel with the log of a runner, which streams while the runner lives.
 * A runner that left the list shows whether it failed, and why.
 */
export function RunnerLogDrawer({ sandbox, stopped, open, onOpenChange }: RunnerLogDrawerProps) {
  const ended = useQuery({ ...sandboxQuery(sandbox.id), enabled: stopped && open }).data
  const failed = stopped && ended?.state === 'failed'
  return (
    <Drawer swipeDirection="right" open={open} onOpenChange={onOpenChange}>
      <DrawerPopup
        title={sandbox.runner_name}
        description={`${failed ? 'Failed' : stopped ? 'Stopped' : sandboxStateNames[sandbox.state]} · ${sandbox.server_name}`}
        // Logs need more room than the 420px of a settings panel.
        width={880}
      >
        <RunnerLog id={sandbox.id} failed={failed ? ended?.error : undefined} />
      </DrawerPopup>
    </Drawer>
  )
}

/** `failed` tells why the runner failed. */
function RunnerLog({ id, failed }: { id: string; failed?: string }) {
  const log = useFollowedText(followSandboxLog(id))

  // A stream can fail after some lines; the error then shows under them.
  const failure = log.error ? (
    <span role="alert" className="block">
      Could not load {log.text ? 'the rest of the log' : 'the log'}: {errorMessage(log.error)}{' '}
      <TextButton onClick={log.retry}>Try again</TextButton>
    </span>
  ) : undefined
  const reason = failed ? (
    <span role="status" className="block">
      Failed: {failed}
    </span>
  ) : undefined
  const notes = failure || reason ? (
    <>
      {reason}
      {failure}
    </>
  ) : undefined

  let empty: ReactNode = log.done ? (notes ?? 'The runner stopped before it wrote a log.') : 'No log output yet.'
  if (log.text === undefined && !log.done) empty = 'Loading the log…'

  return (
    <LogViewer
      log={log.text ?? ''}
      title="Runner log"
      // Tells why the log stopped growing.
      meta={log.done && !log.error && log.text && <span role="status">The runner stopped</span>}
      follow={!log.done}
      downloadUrl={`/api/sandboxes/${id}/log`}
      downloadName={`runner-${id}.log`}
      maxHeight="calc(100dvh - 160px)"
      empty={empty}
      footer={notes}
    />
  )
}
