import { describe, expect, test } from 'vitest'
import { parseAnsi, stripAnsi } from './ansi'

describe('parseAnsi', () => {
  test('splits the line at color codes and resets', () => {
    expect(parseAnsi('\x1b[31mfailed\x1b[0m in 2s').spans).toEqual([
      { text: 'failed', style: { fg: 1 } },
      { text: ' in 2s', style: {} },
    ])
  })

  test('reads bright colors, backgrounds, and text styles together', () => {
    expect(parseAnsi('\x1b[1;92;44mPASS').spans).toEqual([{ text: 'PASS', style: { bold: true, fg: 10, bg: 4 } }])
  })

  test('reads 256-color and 24-bit colors', () => {
    expect(parseAnsi('\x1b[38;5;208mwarn\x1b[48;2;255;0;16mx').spans).toEqual([
      { text: 'warn', style: { fg: 208 } },
      { text: 'x', style: { fg: 208, bg: '#ff0010' } },
    ])
  })

  test('turns single attributes off without dropping the color', () => {
    expect(parseAnsi('\x1b[1;2;33ma\x1b[22mb\x1b[39mc').spans).toEqual([
      { text: 'a', style: { bold: true, dim: true, fg: 3 } },
      { text: 'b', style: { fg: 3 } },
      { text: 'c', style: {} },
    ])
  })

  test('continues the style an earlier line left open, and reports the open style', () => {
    const { spans, end } = parseAnsi('still green\x1b[4m', { fg: 2 })
    expect(spans).toEqual([{ text: 'still green', style: { fg: 2 } }])
    expect(end).toEqual({ fg: 2, underline: true })
  })

  test('drops cursor and erase sequences and hyperlinks but keeps their text', () => {
    const line = '\x1b[2K\x1b[1Gdone \x1b]8;;https://example.com\x07link\x1b]8;;\x07'
    expect(parseAnsi(line).spans).toEqual([{ text: 'done link', style: {} }])
    expect(stripAnsi(line)).toBe('done link')
  })

  test('treats an empty code as a reset', () => {
    expect(parseAnsi('\x1b[31mred\x1b[mplain').spans.at(-1)).toEqual({ text: 'plain', style: {} })
  })
})
