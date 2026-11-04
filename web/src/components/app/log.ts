import { isPlain, parseAnsi, stripAnsi, type AnsiSpan, type AnsiStyle } from './ansi'

// GitHub Actions job logs: each line may start with a timestamp, and the runner
// writes workflow commands as "##[group]", "##[endgroup]", "##[error]", and so on.

export type LogLineKind = 'text' | 'group' | 'command' | 'error' | 'warning' | 'notice' | 'debug'

export type LogLine = {
  /** Line number shown in the gutter. "##[endgroup]" lines are hidden and take no number. */
  number: number
  kind: LogLineKind
  /** Text to show, with ANSI escapes, without the timestamp and command marker. */
  text: string
  /** Text without ANSI escapes, for search and width. */
  plain: string
  /** ANSI style left open by earlier lines; absent when none is. */
  style?: AnsiStyle
  /** Index of the group header this line belongs to. */
  group?: number
  /** For a group header: index of the first line after the group. */
  end?: number
}

export type ParsedLog = {
  lines: LogLine[]
  /** Length of the longest plain line, for the scroll width. */
  width: number
  /** Where parsing stopped after the last complete line, so a longer log can continue from there. */
  resume: Resume
}

type Resume = {
  raw: string
  /** Offset in `raw` after the last newline. */
  offset: number
  /** Lines parsed up to `offset`; the lines after it may still grow. */
  count: number
  width: number
  style: AnsiStyle
  group: number | undefined
}

const timestamp = /^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d(?:\.\d+)?Z /
const command = /^##\[(group|endgroup|error|warning|notice|debug|command|section)\]/
// Failures that test tools print without a workflow command: Playwright's ✘ and "Error:" lines.
const failure = /^\s*(?:✘|Error:)/

const prefixes: Partial<Record<LogLineKind, string>> = { error: 'Error: ', warning: 'Warning: ', notice: 'Notice: ' }

const start: Resume = { raw: '', offset: 0, count: 0, width: 0, style: {}, group: undefined }

/**
 * Parses a job log. With the result for an earlier, shorter version of the same
 * log as `previous`, it parses only the lines after it, as for a log that grows
 * while a job runs.
 */
export function parseLog(raw: string, previous?: ParsedLog): ParsedLog {
  if (raw === previous?.resume.raw) return previous
  const from = previous && raw.startsWith(previous.resume.raw) ? previous.resume : start
  const lines = from === start ? [] : previous!.lines.slice(0, from.count)
  let { width, style, group } = from
  let resume = { ...from, raw }

  for (let at = from.offset; at < raw.length; ) {
    const newline = raw.indexOf('\n', at)
    const lineEnd = newline === -1 ? raw.length : newline
    let text = raw.slice(at === 0 && raw.startsWith('\uFEFF') ? 1 : at, lineEnd).replace(/\r$/, '').replace(timestamp, '')
    at = lineEnd + 1

    const marker = command.exec(stripAnsi(text))?.[1]
    if (marker === 'endgroup') {
      closeGroup(lines, group)
      group = undefined
    } else {
      let kind: LogLineKind = 'text'
      if (marker) {
        text = text.slice(text.indexOf(']') + 1)
        if (marker === 'group') {
          // GitHub does not nest groups: a new group ends the open one.
          closeGroup(lines, group)
          group = undefined
          kind = 'group'
        } else if (marker !== 'section') {
          kind = marker as LogLineKind
        }
      }
      text = (prefixes[kind] ?? '') + text
      const plain = stripAnsi(text)
      if (kind === 'text' && failure.test(plain)) kind = 'error'
      const line: LogLine = { number: lines.length + 1, kind, text, plain }
      if (!isPlain(style)) line.style = style
      if (group !== undefined) line.group = group
      if (text.includes('\x1b')) style = parseAnsi(text, style).end
      if (kind === 'group') group = lines.length
      width = Math.max(width, plain.length)
      lines.push(line)
    }
    if (newline !== -1) resume = { raw, offset: at, count: lines.length, width, style, group }
  }
  closeGroup(lines, group)
  return { lines, width, resume }
}

// A copy, because the line may belong to an earlier result too.
function closeGroup(lines: LogLine[], group: number | undefined) {
  if (group !== undefined && lines[group]!.end !== lines.length) lines[group] = { ...lines[group]!, end: lines.length }
}

/** Groups to open at first: those with an error or warning inside. GitHub shows the rest folded. */
export function groupsWithProblems(lines: readonly LogLine[]): Set<number> {
  const open = new Set<number>()
  for (const line of lines) {
    if (line.group !== undefined && (line.kind === 'error' || line.kind === 'warning')) open.add(line.group)
  }
  return open
}

/** Indexes of the lines on screen: every line, except those inside folded groups. */
export function visibleLines(lines: readonly LogLine[], open: ReadonlySet<number>): number[] {
  const visible: number[] = []
  for (let i = 0; i < lines.length; i++) {
    visible.push(i)
    const line = lines[i]!
    if (line.kind === 'group' && !open.has(i) && line.end !== undefined) i = line.end - 1
  }
  return visible
}

/** Indexes of the lines that contain `query`, ignoring case. An empty query matches nothing. */
export function findLines(lines: readonly LogLine[], query: string): number[] {
  if (!query) return []
  const needle = query.toLowerCase()
  const found: number[] = []
  for (let i = 0; i < lines.length; i++) {
    if (lines[i]!.plain.toLowerCase().includes(needle)) found.push(i)
  }
  return found
}

/** Start and end offsets of each `query` in `text`, ignoring case. */
export function matchRanges(text: string, query: string): Array<[number, number]> {
  if (!query) return []
  const haystack = text.toLowerCase()
  const needle = query.toLowerCase()
  const ranges: Array<[number, number]> = []
  for (let at = haystack.indexOf(needle); at !== -1; at = haystack.indexOf(needle, at + needle.length)) {
    ranges.push([at, at + needle.length])
  }
  return ranges
}

export type LogSegment = AnsiSpan & { match: boolean }

/** Splits styled spans where search matches start and end, so each piece has one style and one match state. */
export function markSpans(spans: readonly AnsiSpan[], ranges: ReadonlyArray<[number, number]>): LogSegment[] {
  const segments: LogSegment[] = []
  let offset = 0
  for (const span of spans) {
    const end = offset + span.text.length
    // Cut points inside this span: range starts and ends that fall within it.
    const cuts = new Set([offset, end])
    for (const [from, to] of ranges) {
      if (from > offset && from < end) cuts.add(from)
      if (to > offset && to < end) cuts.add(to)
    }
    const points = [...cuts].sort((a, b) => a - b)
    for (let i = 0; i < points.length - 1; i++) {
      const from = points[i]!
      const to = points[i + 1]!
      const match = ranges.some(([start, stop]) => from >= start && to <= stop)
      segments.push({ text: span.text.slice(from - offset, to - offset), style: span.style, match })
    }
    offset = end
  }
  return segments
}
