import { ChevronDown, ChevronRight, ChevronUp, Download } from 'lucide-react'
import { useEffect, useLayoutEffect, useMemo, useRef, useState, type CSSProperties, type ReactNode } from 'react'
import { IconButton } from '~/components/ui/button'
import { SearchInput } from '~/components/ui/input'
import { cx } from '~/lib/cx'
import { parseAnsi, type AnsiColor, type AnsiStyle } from './ansi'
import { findLines, groupsWithProblems, markSpans, matchRanges, parseLog, visibleLines, type LogLine, type ParsedLog } from './log'

type LogViewerProps = {
  /** The raw log. It may grow while `follow` is set. */
  log: string
  /** Heading of the panel, for example the step name. */
  title?: ReactNode
  /** Muted text after the title, for example the step duration. */
  meta?: ReactNode
  /** Keeps the newest line in view while the user stays at the bottom. */
  follow?: boolean
  /** Downloads this URL instead of the text in memory, for example the full job log. */
  downloadUrl?: string
  downloadName?: string
  /** CSS height limit of the lines; past it they scroll. */
  maxHeight?: string
  /** Shown when the log has no lines. */
  empty?: ReactNode
  /** Shown under the lines, for example why the log stopped. */
  footer?: ReactNode
  className?: string
}

const overscan = 20

/**
 * The job log of Job detail: line numbers, ANSI colors, GitHub "##[group]"
 * sections that fold, highlighted errors and test failures, search, and
 * download. Only the lines on screen are rendered, so logs of 100,000 lines
 * scroll smoothly, and a growing log parses only its new lines.
 */
export function LogViewer({
  log,
  title,
  meta,
  follow = false,
  downloadUrl,
  downloadName = 'log.txt',
  maxHeight = '70vh',
  empty = 'No log output yet.',
  footer,
  className,
}: LogViewerProps) {
  const { lines, width } = useParsedLog(log)
  const [open, setOpen] = useState<ReadonlySet<number>>(() => groupsWithProblems(lines))
  const [query, setQuery] = useState('')
  const [current, setCurrent] = useState(0)
  const visible = useMemo(() => visibleLines(lines, open), [lines, open])
  const matches = useMemo(() => findLines(lines, query), [lines, query])
  const target = matches.length > 0 ? matches[Math.min(current, matches.length - 1)] : undefined

  // Groups that receive an error while the log grows open, as at the start.
  useEffect(() => {
    setOpen((previous) => {
      const problems = groupsWithProblems(lines)
      return [...problems].every((group) => previous.has(group)) ? previous : new Set([...previous, ...problems])
    })
  }, [lines])

  const scroller = useRef<HTMLDivElement>(null)
  const [viewport, setViewport] = useState({ top: 0, height: 0, rowHeight: 18 })
  const atBottom = useRef(true)
  // The match last scrolled into view; a new match, or a press of Enter, scrolls again.
  const shown = useRef<number | undefined>(undefined)
  const hasLines = lines.length > 0

  useLayoutEffect(() => {
    const element = scroller.current
    if (!element) return
    // Rows are one --line-s high; read it from the theme so the math follows it.
    const rowHeight = parseFloat(getComputedStyle(element).getPropertyValue('--line-s')) || 18
    const measure = () => setViewport({ top: element.scrollTop, height: element.clientHeight, rowHeight })
    measure()
    const observer = new ResizeObserver(measure)
    observer.observe(element)
    return () => observer.disconnect()
  }, [hasLines])

  useLayoutEffect(() => {
    const element = scroller.current
    if (follow && element && atBottom.current) element.scrollTop = element.scrollHeight
  }, [follow, visible.length])

  // Bring the current search match into view, opening its group first.
  useEffect(() => {
    if (target === undefined || shown.current === target) return
    const group = lines[target]!.group
    if (group !== undefined && !open.has(group)) {
      setOpen(new Set([...open, group]))
      return
    }
    const element = scroller.current
    const row = binarySearch(visible, target)
    if (!element || row === -1) return
    const y = row * viewport.rowHeight
    if (y < element.scrollTop || y + viewport.rowHeight > element.scrollTop + element.clientHeight) {
      element.scrollTop = y - element.clientHeight / 2
    }
    shown.current = target
  }, [target, open, lines, visible, viewport.rowHeight])

  const { rowHeight } = viewport
  const first = Math.max(0, Math.floor(viewport.top / rowHeight) - overscan)
  const last = Math.min(visible.length, Math.ceil((viewport.top + viewport.height) / rowHeight) + overscan)
  const hasGroups = lines.some((line) => line.kind === 'group')
  const gutter = `max(20px, ${String(lines.length).length}ch)`

  function toggle(index: number) {
    const next = new Set(open)
    if (!next.delete(index)) next.add(index)
    setOpen(next)
  }

  function step(by: number) {
    shown.current = undefined
    if (matches.length > 0) setCurrent((Math.min(current, matches.length - 1) + by + matches.length) % matches.length)
  }

  function download() {
    const url = URL.createObjectURL(new Blob([log], { type: 'text/plain' }))
    const link = document.createElement('a')
    link.href = url
    link.download = downloadName
    link.click()
    URL.revokeObjectURL(url)
  }

  return (
    <section className={cx('flex min-w-0 flex-col rounded-m bg-neutral-2 pb-l', className)}>
      <header className="flex flex-wrap items-center gap-m py-m pr-m pl-l">
        {/* At least 160px, so on a narrow screen the search goes to the next line instead of hiding the title. */}
        <div className="flex min-w-[160px] grow basis-0 items-baseline gap-xs">
          {title && <h2 className="truncate text-m font-heavy tracking-l text-neutral-10">{title}</h2>}
          {meta && <span className="shrink-0 text-s text-neutral-7">{meta}</span>}
        </div>
        <div className="flex items-center gap-m">
          <SearchInput
            aria-label="Find in logs"
            placeholder="Find in logs"
            value={query}
            className="w-[220px]"
            onChange={(event) => {
              shown.current = undefined
              setQuery(event.target.value)
              setCurrent(0)
            }}
            onKeyDown={(event) => {
              if (event.key !== 'Enter') return
              event.preventDefault()
              step(event.shiftKey ? -1 : 1)
            }}
          />
          {query && (
            <div className="flex items-center gap-xs">
              <span className="min-w-[7ch] text-xs text-neutral-7 tabular-nums" aria-live="polite">
                {matches.length === 0 ? 'No matches' : `${Math.min(current, matches.length - 1) + 1} of ${matches.length}`}
              </span>
              <IconButton label="Previous match" disabled={matches.length === 0} onClick={() => step(-1)}>
                <ChevronUp aria-hidden />
              </IconButton>
              <IconButton label="Next match" disabled={matches.length === 0} onClick={() => step(1)}>
                <ChevronDown aria-hidden />
              </IconButton>
            </div>
          )}
          {downloadUrl ? (
            <IconButton label="Download log" nativeButton={false} render={<a href={downloadUrl} download={downloadName} />}>
              <Download aria-hidden />
            </IconButton>
          ) : (
            <IconButton label="Download log" onClick={download} disabled={lines.length === 0}>
              <Download aria-hidden />
            </IconButton>
          )}
        </div>
      </header>
      {!hasLines ? (
        <p className="px-l text-s text-neutral-7">{empty}</p>
      ) : (
        <div
          ref={scroller}
          role="region"
          aria-label={typeof title === 'string' ? `${title} log` : 'Log'}
          tabIndex={0}
          className="overflow-auto overscroll-contain font-code text-xs leading-s tracking-m outline-none focus-visible:outline-2 focus-visible:-outline-offset-2"
          style={{ maxHeight }}
          onScroll={(event) => {
            const element = event.currentTarget
            atBottom.current = element.scrollTop + element.clientHeight >= element.scrollHeight - rowHeight
            setViewport((previous) => ({ ...previous, top: element.scrollTop }))
          }}
        >
          <div
            className="relative"
            style={{
              height: visible.length * rowHeight,
              // Every line keeps one row, so the widest line sets the scroll width.
              minWidth: `calc(${width}ch + ${gutter} + ${hasGroups ? '12px + var(--space-m) + ' : ''}var(--space-m) + 2 * var(--space-l))`,
            }}
          >
            <div className="absolute inset-x-zero top-zero" style={{ transform: `translateY(${first * rowHeight}px)` }}>
              {visible.slice(first, last).map((index) => (
                <Row
                  key={index}
                  line={lines[index]!}
                  gutter={gutter}
                  hasGroups={hasGroups}
                  open={open.has(index)}
                  query={query}
                  current={index === target}
                  onToggle={() => toggle(index)}
                />
              ))}
            </div>
          </div>
        </div>
      )}
      {hasLines && footer && <p className="px-l pt-m text-s text-neutral-7">{footer}</p>}
    </section>
  )
}

// Parses only the new end of a log that grows, as one that `follow` streams.
function useParsedLog(log: string): ParsedLog {
  const last = useRef<ParsedLog | undefined>(undefined)
  const parsed = useMemo(() => parseLog(log, last.current), [log])
  useEffect(() => {
    last.current = parsed
  }, [parsed])
  return parsed
}

type RowProps = {
  line: LogLine
  gutter: string
  hasGroups: boolean
  open: boolean
  query: string
  current: boolean
  onToggle: () => void
}

function Row({ line, gutter, hasGroups, open, query, current, onToggle }: RowProps) {
  const problem = line.kind === 'error' || line.kind === 'warning'
  const className = cx(
    'flex h-(--line-s) w-full gap-m px-l text-left whitespace-pre',
    problem ? 'bg-neutral-3 text-neutral-10' : line.kind === 'debug' ? 'text-neutral-6' : 'text-neutral-8',
    (line.kind === 'command' || line.kind === 'group') && 'text-neutral-10',
    current && 'bg-color-1-transparent',
  )
  const content = (
    <>
      <span className="shrink-0 text-right text-neutral-6 select-none" style={{ width: gutter }}>
        {line.number}
      </span>
      {hasGroups && (
        <span className="flex w-[12px] shrink-0 items-center text-neutral-7" aria-hidden>
          {line.kind === 'group' && (open ? <ChevronDown size={12} /> : <ChevronRight size={12} />)}
        </span>
      )}
      <LineText line={line} query={query} />
    </>
  )
  if (line.kind === 'group') {
    return (
      <button type="button" aria-expanded={open} className={cx(className, 'cursor-pointer hover:bg-neutral-3')} onClick={onToggle}>
        {content}
      </button>
    )
  }
  return <div className={className}>{content}</div>
}

function LineText({ line, query }: { line: LogLine; query: string }) {
  const spans = line.style || line.text.includes('\x1b') ? parseAnsi(line.text, line.style).spans : [{ text: line.text, style: {} }]
  const segments = markSpans(spans, matchRanges(line.plain, query))
  return (
    <span>
      {segments.map((segment, index) =>
        segment.match ? (
          <mark key={index} className="rounded-xs bg-color-1 text-neutral-1" style={ansiCss(segment.style, false)}>
            {segment.text}
          </mark>
        ) : (
          <span key={index} style={ansiCss(segment.style, true)}>
            {segment.text}
          </span>
        ),
      )}
    </span>
  )
}

function ansiCss(style: AnsiStyle, withColor: boolean): CSSProperties | undefined {
  if (Object.keys(style).length === 0) return undefined
  return {
    color: withColor && style.fg !== undefined ? ansiColor(style.fg) : undefined,
    // A tint of the color keeps the text on it readable in both modes.
    backgroundColor: withColor && style.bg !== undefined ? `color-mix(in srgb, ${ansiColor(style.bg)} 30%, transparent)` : undefined,
    fontWeight: style.bold ? 'var(--weight-ui-heavy)' : undefined,
    opacity: style.dim ? 0.7 : undefined,
    fontStyle: style.italic ? 'italic' : undefined,
    textDecoration: style.underline ? 'underline' : undefined,
  }
}

/**
 * The 16 terminal colors. The theme has no hues for red, green, or yellow (its
 * status colors are black and white), so these are their own values, chosen
 * for contrast on the neutral-2 log panel in each mode. Black and white map to
 * neutrals, so they stay readable in both modes.
 */
export const terminalColors = [
  'var(--neutral-7)', // black
  'light-dark(#c4262e, #ff7b72)', // red
  'light-dark(#1a7f37, #56d364)', // green
  'light-dark(#8a5a00, #e3b341)', // yellow
  'light-dark(#2759cc, #79a8ff)', // blue
  'light-dark(#9a2b9a, #e48ae0)', // magenta
  'light-dark(#0b7285, #56d4dd)', // cyan
  'var(--neutral-7)', // white
  'var(--neutral-6)', // bright black: the usual gray for quiet text
  'light-dark(#d1242f, #ffa198)',
  'light-dark(#1f883d, #7ee787)',
  'light-dark(#9a6700, #f2cc60)',
  'light-dark(#3b6fe0, #a5c3ff)',
  'light-dark(#b13cb1, #f0a5ec)',
  'light-dark(#0e8599, #7ee3ea)',
  'var(--neutral-10)', // bright white
]

function ansiColor(color: AnsiColor): string {
  if (typeof color === 'string') return color
  if (color < 16) return terminalColors[color]!
  if (color >= 232) {
    const gray = 8 + (color - 232) * 10
    return `rgb(${gray} ${gray} ${gray})`
  }
  // The 6×6×6 color cube of xterm.
  const levels = [0, 95, 135, 175, 215, 255]
  const cube = color - 16
  return `rgb(${levels[Math.floor(cube / 36)]} ${levels[Math.floor(cube / 6) % 6]} ${levels[cube % 6]})`
}

function binarySearch(sorted: readonly number[], value: number): number {
  let low = 0
  let high = sorted.length - 1
  while (low <= high) {
    const middle = (low + high) >> 1
    const found = sorted[middle]!
    if (found === value) return middle
    if (found < value) low = middle + 1
    else high = middle - 1
  }
  return -1
}
