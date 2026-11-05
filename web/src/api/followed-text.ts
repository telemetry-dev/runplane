import { useEffect, useEffectEvent, useState } from 'react'
import { usePageShows } from '~/lib/use-page-shows'

/** A request whose response sends text until it ends, as a log with ?follow=1. */
export type FollowedRequest = {
  /** Names the text: another key starts with empty text. */
  key: string
  open: (signal: AbortSignal) => Promise<Response['body'] | undefined>
}

export type FollowedText = {
  /** undefined until the first bytes arrive. */
  text: string | undefined
  error: unknown
  /** The request ended, and it does not open again. */
  done: boolean
  retry: () => void
}

type State = Omit<FollowedText, 'retry'> & { key: string }

const reconnectMs = 3_000

/**
 * The text of a followed request. Bursts of chunks show in one change. With
 * `reconnect`, a request that ends or fails opens again after 3 seconds, and
 * the text on screen stays until the new request sends its own. When
 * `enabled` turns off, the text of the last request stays.
 *
 * The request ends when the page goes into the back/forward cache, and opens
 * again when the page comes back. The cached page keeps it open otherwise,
 * and browsers allow only six connections to one host over HTTP/1.1, so a few
 * cached pages stall the app.
 */
export function useFollowedText(request: FollowedRequest, { enabled = true, reconnect = false } = {}): FollowedText {
  const [state, setState] = useState<State>({ key: '', text: undefined, error: null, done: false })
  const [attempt, setAttempt] = useState(0)
  const shows = usePageShows()
  const { key } = request
  const open = useEffectEvent((signal: AbortSignal) => request.open(signal))

  useEffect(() => {
    if (!enabled) return
    const controller = new AbortController()
    const abort = () => controller.abort()
    addEventListener('pagehide', abort)
    let text = ''
    let batch: ReturnType<typeof setTimeout> | undefined
    let again: ReturnType<typeof setTimeout> | undefined
    const show = () => {
      batch = undefined
      setState({ key, text, error: null, done: false })
    }
    const end = (next: (previous: State) => State) => {
      if (reconnect) again = setTimeout(() => setAttempt((count) => count + 1), reconnectMs)
      setState(next)
    }
    setState((previous) => (previous.key === key ? { ...previous, error: null, done: false } : { key, text: undefined, error: null, done: false }))

    async function follow() {
      const body = await open(controller.signal)
      if (body) {
        const reader = body.pipeThrough(new TextDecoderStream()).getReader()
        for (;;) {
          const { value, done } = await reader.read()
          if (done) break
          text += value
          batch ??= setTimeout(show, 200)
        }
      }
      clearTimeout(batch)
    }

    follow().then(
      () => end(() => ({ key, text, error: null, done: !reconnect })),
      (error: unknown) => {
        if (controller.signal.aborted) return
        clearTimeout(batch)
        end((previous) => ({ key, text: text || (previous.key === key ? previous.text : undefined), error, done: !reconnect }))
      },
    )
    return () => {
      removeEventListener('pagehide', abort)
      controller.abort()
      clearTimeout(batch)
      clearTimeout(again)
    }
  }, [key, enabled, reconnect, attempt, shows])

  const current = state.key === key ? state : undefined
  return {
    text: current?.text,
    error: current?.error ?? null,
    done: current?.done ?? false,
    retry: () => setAttempt((count) => count + 1),
  }
}
