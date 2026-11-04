import { describe, expect, test } from 'vitest'
import { findLines, groupsWithProblems, markSpans, matchRanges, parseLog, visibleLines } from './log'

const log = [
  '﻿2026-11-04T10:00:00.1234567Z ##[group]Run actions/checkout@v4',
  '2026-11-04T10:00:00.2000000Z with:',
  '2026-11-04T10:00:00.3000000Z   repository: kestrel-hq/web',
  '2026-11-04T10:00:00.4000000Z ##[endgroup]',
  '2026-11-04T10:00:01.0000000Z ##[command]/usr/bin/git fetch',
  '2026-11-04T10:00:02.0000000Z ##[group]Run pnpm test',
  '2026-11-04T10:00:03.0000000Z \x1b[32m✓ cart\x1b[0m',
  '2026-11-04T10:00:04.0000000Z ##[error]Process completed with exit code 1.',
  '2026-11-04T10:00:05.0000000Z ##[endgroup]',
  'Cleaning up',
  '',
].join('\n')

describe('parseLog', () => {
  const { lines, width } = parseLog(log)

  test('drops timestamps, markers, and endgroup lines, and numbers the rest in order', () => {
    expect(lines.map((line) => [line.number, line.kind, line.plain])).toEqual([
      [1, 'group', 'Run actions/checkout@v4'],
      [2, 'text', 'with:'],
      [3, 'text', '  repository: kestrel-hq/web'],
      [4, 'command', '/usr/bin/git fetch'],
      [5, 'group', 'Run pnpm test'],
      [6, 'text', '✓ cart'],
      [7, 'error', 'Error: Process completed with exit code 1.'],
      [8, 'text', 'Cleaning up'],
    ])
  })

  test('links group members to their header and records where each group ends', () => {
    expect(lines.map((line) => line.group)).toEqual([undefined, 0, 0, undefined, undefined, 4, 4, undefined])
    expect(lines[0]!.end).toBe(3)
    expect(lines[4]!.end).toBe(7)
  })

  test('keeps the ANSI codes in the text and measures the width without them', () => {
    expect(lines[5]!.text).toBe('\x1b[32m✓ cart\x1b[0m')
    expect(width).toBe('Error: Process completed with exit code 1.'.length)
  })

  test('a new group ends the open one', () => {
    const parsed = parseLog('##[group]a\none\n##[group]b\ntwo\n').lines
    expect(parsed.map((line) => line.group)).toEqual([undefined, 0, undefined, 2])
    expect(parsed[0]!.end).toBe(2)
    expect(parsed[2]!.end).toBe(4)
  })

  test('carries a color that a line leaves open into the next lines', () => {
    const parsed = parseLog('\x1b[31mstart\nmiddle\x1b[0m\nend').lines
    expect(parsed.map((line) => line.style)).toEqual([undefined, { fg: 1 }, undefined])
  })
})

describe('parseLog problem lines', () => {
  test('marks Playwright failures and Error: lines as errors, but not passes or other text', () => {
    const parsed = parseLog(
      [
        '  ✓  cart.spec.ts:14:3 › adds item to cart (2.1s)',
        '  ✘  redirect.spec.ts:22:3 › returns to order page (30.0s)',
        '     Error: Timed out 30000ms waiting for expect(page).toHaveURL()',
        'Errors: 0',
        '  1 failed',
      ].join('\n'),
    ).lines
    expect(parsed.map((line) => line.kind)).toEqual(['text', 'error', 'error', 'text', 'text'])
    expect(parsed[2]!.plain).toBe('     Error: Timed out 30000ms waiting for expect(page).toHaveURL()')
  })

  test('opens the group around a Playwright failure', () => {
    const { lines } = parseLog('##[group]Run pnpm e2e\n  ✘  a.spec.ts › fails\n##[endgroup]\n##[group]Run lint\nok\n')
    expect([...groupsWithProblems(lines)]).toEqual([0])
  })
})

describe('parseLog with a growing log', () => {
  // Cuts inside a timestamp, an ANSI escape, a group marker, and a CRLF.
  const full = [
    '2026-11-04T10:00:00.1Z ##[group]Run tests',
    '\x1b[31mred start',
    'still red\x1b[0m\r',
    '##[endgroup]',
    '##[group]Run e2e',
    '  ✘  checkout fails',
    'tail without newline',
  ].join('\n')
  const cuts = [7, 30, 47, 58, full.indexOf('\r') + 1, full.indexOf('##[endgroup]') + 5, full.indexOf('✘'), full.length - 4, full.length]

  test('gives the same lines as parsing the whole log at once', () => {
    let parsed = parseLog('')
    for (const cut of cuts) parsed = parseLog(full.slice(0, cut), parsed)
    const whole = parseLog(full)
    expect(parsed.lines).toEqual(whole.lines)
    expect(parsed.width).toBe(whole.width)
  })

  test('keeps the lines it parsed before and parses only the new ones', () => {
    const first = parseLog('one\ntwo\n')
    const next = parseLog('one\ntwo\nthree\n', first)
    expect(next.lines[0]).toBe(first.lines[0])
    expect(next.lines.map((line) => line.plain)).toEqual(['one', 'two', 'three'])
  })

  test('parses a different log from the start', () => {
    const first = parseLog('one\ntwo\n')
    expect(parseLog('other\n', first).lines.map((line) => line.plain)).toEqual(['other'])
  })
})

describe('visibleLines', () => {
  const { lines } = parseLog(log)

  test('hides the members of folded groups but keeps their headers', () => {
    expect(visibleLines(lines, new Set())).toEqual([0, 3, 4, 7])
  })

  test('shows the members of open groups', () => {
    expect(visibleLines(lines, new Set([4]))).toEqual([0, 3, 4, 5, 6, 7])
  })

  test('opens only the groups that hold an error or warning', () => {
    expect([...groupsWithProblems(lines)]).toEqual([4])
  })
})

describe('search', () => {
  const { lines } = parseLog(log)

  test('finds lines by their text without ANSI codes, ignoring case', () => {
    expect(findLines(lines, 'CART')).toEqual([5])
    expect(findLines(lines, 'run')).toEqual([0, 4])
    expect(findLines(lines, '')).toEqual([])
  })

  test('returns each match in a line without overlap', () => {
    expect(matchRanges('aaa Aa', 'aa')).toEqual([
      [0, 2],
      [4, 6],
    ])
  })
})

describe('markSpans', () => {
  test('cuts styled spans at the edges of matches that cross them', () => {
    const spans = [
      { text: 'red ', style: { fg: 1 } },
      { text: 'plain', style: {} },
    ]
    expect(markSpans(spans, [[2, 6]])).toEqual([
      { text: 're', style: { fg: 1 }, match: false },
      { text: 'd ', style: { fg: 1 }, match: true },
      { text: 'pl', style: {}, match: true },
      { text: 'ain', style: {}, match: false },
    ])
  })

  test('leaves spans whole when nothing matches', () => {
    expect(markSpans([{ text: 'abc', style: {} }], [])).toEqual([{ text: 'abc', style: {}, match: false }])
  })
})
