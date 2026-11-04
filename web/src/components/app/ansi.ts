// ANSI escape codes in job logs: SGR colors and text styles. Other escape
// sequences (cursor moves, line erase, OSC hyperlinks) are dropped.

/** A palette index from 0 to 255, or a #rrggbb color from a 24-bit code. */
export type AnsiColor = number | string

export type AnsiStyle = {
  fg?: AnsiColor
  bg?: AnsiColor
  bold?: boolean
  dim?: boolean
  italic?: boolean
  underline?: boolean
}

export type AnsiSpan = { text: string; style: AnsiStyle }

// CSI sequences (ESC [ params letter) and OSC sequences (ESC ] … BEL or ESC \).
const escapes = /\x1b\[([0-9;:?]*)([@-~])|\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)/g

/**
 * Splits a line into runs of text with one style each. `start` is the style
 * that earlier lines left open; `end` is the style open after this line.
 */
export function parseAnsi(line: string, start: AnsiStyle = {}): { spans: AnsiSpan[]; end: AnsiStyle } {
  const spans: AnsiSpan[] = []
  let style = start
  let last = 0
  for (const match of line.matchAll(escapes)) {
    if (match.index > last) push(spans, line.slice(last, match.index), style)
    last = match.index + match[0].length
    if (match[2] === 'm') style = applySgr(style, match[1] ?? '')
  }
  if (last < line.length) push(spans, line.slice(last), style)
  return { spans, end: style }
}

/** The line without escape sequences. */
export function stripAnsi(line: string): string {
  return line.includes('\x1b') ? line.replace(escapes, '') : line
}

export function isPlain(style: AnsiStyle): boolean {
  return Object.keys(style).length === 0
}

function push(spans: AnsiSpan[], text: string, style: AnsiStyle) {
  const previous = spans.at(-1)
  if (previous && previous.style === style) previous.text += text
  else spans.push({ text, style })
}

function applySgr(current: AnsiStyle, params: string): AnsiStyle {
  // "ESC[m" is a reset, as "ESC[0m". Colon separators (38:5:n) work like semicolons.
  const codes = params === '' ? [0] : params.split(/[;:]/).map((code) => (code === '' ? 0 : Number(code)))
  const style: AnsiStyle = { ...current }
  for (let i = 0; i < codes.length; i++) {
    const code = codes[i]!
    if (code === 0) {
      for (const key of Object.keys(style)) delete style[key as keyof AnsiStyle]
    } else if (code === 1) style.bold = true
    else if (code === 2) style.dim = true
    else if (code === 3) style.italic = true
    else if (code === 4) style.underline = true
    else if (code === 22) {
      delete style.bold
      delete style.dim
    } else if (code === 23) delete style.italic
    else if (code === 24) delete style.underline
    else if (code >= 30 && code <= 37) style.fg = code - 30
    else if (code >= 90 && code <= 97) style.fg = code - 90 + 8
    else if (code === 39) delete style.fg
    else if (code >= 40 && code <= 47) style.bg = code - 40
    else if (code >= 100 && code <= 107) style.bg = code - 100 + 8
    else if (code === 49) delete style.bg
    else if (code === 38 || code === 48) {
      const [color, used] = extendedColor(codes, i + 1)
      if (color !== undefined) style[code === 38 ? 'fg' : 'bg'] = color
      i += used
    }
  }
  return style
}

// 5;n is a palette index, 2;r;g;b a 24-bit color. Returns the color and how many codes it used.
function extendedColor(codes: number[], at: number): [AnsiColor | undefined, number] {
  if (codes[at] === 5) {
    const index = codes[at + 1]
    return [index !== undefined && index >= 0 && index <= 255 ? index : undefined, 2]
  }
  if (codes[at] === 2) {
    const rgb = codes.slice(at + 1, at + 4)
    if (rgb.length < 3 || rgb.some((value) => value < 0 || value > 255)) return [undefined, 4]
    return [`#${rgb.map((value) => value.toString(16).padStart(2, '0')).join('')}`, 4]
  }
  return [undefined, 0]
}
