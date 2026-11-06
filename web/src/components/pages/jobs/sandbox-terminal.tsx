import { FitAddon } from '@xterm/addon-fit'
import { Terminal, type ITheme } from '@xterm/xterm'
import '@xterm/xterm/css/xterm.css'
import { useEffect, useRef, useState } from 'react'
import { apiUrl } from '~/api/client'
import { terminalColors } from '~/components/app/log-viewer'
import { TextButton } from '~/components/app/text-button'

type Status = { state: 'connecting' } | { state: 'open' } | { state: 'closed'; message: string }

/**
 * A browser terminal on GET /api/sandboxes/{id}/shell. Keys go to the shell as
 * binary frames and output comes back the same way; a size change goes as the
 * text frame {"type":"resize","rows":N,"cols":N}. Escape belongs to the shell,
 * so Shift+Tab moves the focus out of the terminal.
 */
export default function SandboxTerminal({ sandboxId }: { sandboxId: string }) {
  const host = useRef<HTMLDivElement>(null)
  const [status, setStatus] = useState<Status>({ state: 'connecting' })
  const [attempt, setAttempt] = useState(0)

  useEffect(() => {
    const element = host.current!
    let stop = () => {}
    let cancelled = false
    setStatus({ state: 'connecting' })

    const style = getComputedStyle(element)
    const fontFamily = style.getPropertyValue('--font-code')
    const fontSize = parseFloat(style.getPropertyValue('--size-s'))
    // xterm measures one character when it opens, so the font must be ready first.
    void document.fonts.load(`${fontSize}px ${fontFamily}`).then(() => {
      if (cancelled) return
      stop = connect(element, sandboxId, { fontFamily, fontSize, theme: theme(element) }, setStatus)
    })
    return () => {
      cancelled = true
      stop()
    }
  }, [sandboxId, attempt])

  return (
    <div className="flex flex-col gap-s">
      {/* xterm.css (unlayered, so it beats utilities) fills the viewport black, which shows as lines at the edges. */}
      <div className="h-[min(480px,60dvh)] rounded-s bg-(--code-background) p-s [&_.xterm-viewport]:bg-(--code-background)!">
        <div ref={host} className="size-full" />
      </div>
      <div className="flex flex-wrap items-center justify-between gap-m text-xs text-neutral-7">
        <p role="status">
          {status.state === 'connecting' && 'Connecting…'}
          {status.state === 'open' && 'Connected'}
          {status.state === 'closed' && (
            <>
              {status.message}{' '}
              <TextButton onClick={() => setAttempt((n) => n + 1)}>Reconnect</TextButton>
            </>
          )}
        </p>
        <p>Shift+Tab leaves the terminal.</p>
      </div>
    </div>
  )
}

function connect(
  element: HTMLElement,
  sandboxId: string,
  options: { fontFamily: string; fontSize: number; theme: ITheme },
  setStatus: (status: Status) => void,
): () => void {
  const term = new Terminal({ ...options, cursorBlink: true, scrollback: 5000 })
  const fit = new FitAddon()
  term.loadAddon(fit)
  term.open(element)
  fit.fit()
  term.attachCustomKeyEventHandler((event) => !(event.key === 'Tab' && event.shiftKey))

  const shell = apiUrl('/api/sandboxes/{id}/shell', { path: { id: sandboxId }, query: { rows: term.rows, cols: term.cols } })
  const url = new URL(shell, window.location.href)
  url.protocol = url.protocol === 'https:' ? 'wss:' : 'ws:'
  const socket = new WebSocket(url)
  socket.binaryType = 'arraybuffer'
  let opened = false

  const send = (data: string | Uint8Array) => {
    if (socket.readyState === WebSocket.OPEN) socket.send(data)
  }
  const encoder = new TextEncoder()
  socket.onopen = () => {
    opened = true
    setStatus({ state: 'open' })
    term.focus()
  }
  socket.onmessage = (event: MessageEvent<ArrayBuffer | string>) => {
    term.write(typeof event.data === 'string' ? event.data : new Uint8Array(event.data))
  }
  socket.onclose = (event) => {
    const message = event.reason || (opened ? 'The shell ended.' : 'Could not connect to the sandbox.')
    setStatus({ state: 'closed', message })
  }
  const subscriptions = [
    term.onData((data) => send(encoder.encode(data))),
    // Some mouse reports are bytes that are not valid UTF-8.
    term.onBinary((data) => send(Uint8Array.from(data, (char) => char.charCodeAt(0)))),
    term.onResize(({ rows, cols }) => send(JSON.stringify({ type: 'resize', rows, cols }))),
  ]
  const observer = new ResizeObserver(() => fit.fit())
  observer.observe(element)

  return () => {
    observer.disconnect()
    for (const subscription of subscriptions) subscription.dispose()
    socket.onclose = null
    socket.close()
    term.dispose()
  }
}

/**
 * Terminal colors of the log panel. xterm cannot read CSS variables or
 * light-dark(), so a probe element resolves each one for the current mode.
 */
function theme(element: HTMLElement): ITheme {
  const probe = document.createElement('span')
  element.append(probe)
  const resolve = (value: string) => {
    probe.style.color = value
    return getComputedStyle(probe).color
  }
  const ansi = Object.fromEntries(ansiNames.map((name, index) => [name, resolve(terminalColors[index]!)]))
  const colors: ITheme = {
    ...ansi,
    background: resolve('var(--code-background)'),
    foreground: resolve('var(--neutral-9)'),
    cursor: resolve('var(--neutral-10)'),
    cursorAccent: resolve('var(--code-background)'),
    selectionBackground: resolve('var(--color-1-transparent)'),
  }
  probe.remove()
  return colors
}

const ansiNames = [
  'black',
  'red',
  'green',
  'yellow',
  'blue',
  'magenta',
  'cyan',
  'white',
  'brightBlack',
  'brightRed',
  'brightGreen',
  'brightYellow',
  'brightBlue',
  'brightMagenta',
  'brightCyan',
  'brightWhite',
] as const
