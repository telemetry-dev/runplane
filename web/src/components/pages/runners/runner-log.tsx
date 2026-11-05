import { useQuery } from '@tanstack/react-query'
import { useEffect, useState, type ReactNode } from 'react'
import { $api, api, errorMessage } from '~/api/client'
import { abortOnPageHide, usePageShows } from '~/api/open-requests'
import { LogViewer } from '~/components/app/log-viewer'
import { TextButton } from '~/components/app/text-button'
import { Drawer, DrawerPopup } from '~/components/ui/drawer'
import { stateNames, type Sandbox } from './runner-format'

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
  const ended = useQuery({
    ...$api.queryOptions('get', '/api/sandboxes/{id}', { params: { path: { id: sandbox.id } } }),
    enabled: stopped && open,
  }).data
  const failed = stopped && ended?.state === 'failed'
  return (
    <Drawer swipeDirection="right" open={open} onOpenChange={onOpenChange}>
      <DrawerPopup
        title={sandbox.runner_name}
        description={`${failed ? 'Failed' : stopped ? 'Stopped' : stateNames[sandbox.state]} · ${sandbox.server_name}`}
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
  const log = useRunnerLog(id)

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

type RunnerLogText = {
  /** undefined until the first bytes arrive. */
  text: string | undefined
  error: unknown
  /** The stream ended, because the runner stopped or the request failed. */
  done: boolean
}

/** Streams GET /api/sandboxes/{id}/log?follow=1 until the runner stops or the panel closes. */
function useRunnerLog(id: string) {
  const [log, setLog] = useState<RunnerLogText>({ text: undefined, error: null, done: false })
  const [attempt, setAttempt] = useState(0)
  const shows = usePageShows()

  useEffect(() => {
    const controller = new AbortController()
    const release = abortOnPageHide(controller)
    let text = ''
    // LogViewer parses the whole log on each change, so batch the chunks of a burst.
    let timer: ReturnType<typeof setTimeout> | undefined
    const show = () => {
      timer = undefined
      setLog({ text, error: null, done: false })
    }
    setLog({ text: undefined, error: null, done: false })

    async function stream() {
      const { data } = await api.GET('/api/sandboxes/{id}/log', {
        params: { path: { id }, query: { follow: true } },
        parseAs: 'stream',
        signal: controller.signal,
      })
      if (data) {
        const reader = data.pipeThrough(new TextDecoderStream()).getReader()
        for (;;) {
          const { value, done } = await reader.read()
          if (done) break
          text += value
          timer ??= setTimeout(show, 200)
        }
      }
      clearTimeout(timer)
      setLog({ text, error: null, done: true })
    }

    stream().catch((error: unknown) => {
      if (!controller.signal.aborted) setLog({ text: text || undefined, error, done: true })
    })
    return () => {
      release()
      controller.abort()
      clearTimeout(timer)
    }
  }, [id, attempt, shows])

  return { ...log, retry: () => setAttempt((count) => count + 1) }
}
