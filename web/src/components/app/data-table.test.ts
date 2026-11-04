import { describe, expect, test } from 'vitest'
import { fitColumns } from './data-table'

// The columns of the Servers table. At 12px gaps all of them need 810px inside
// the 1px edges, 628px without Resources, 476px also without Memory, and 324px
// also without CPU.
const servers = {
  status: 16,
  server: 'grow',
  resources: { width: 170, drop: 1 },
  cpu: { width: 140, drop: 3 },
  memory: { width: 140, drop: 2 },
  running: 64,
  menu: 24,
} as const

describe('fitColumns', () => {
  test.each([
    { width: 0, columns: [16, 'grow', 170, 140, 140, 64, 24] },
    { width: 812, columns: [16, 'grow', 170, 140, 140, 64, 24] },
    { width: 811, columns: [16, 'grow', 140, 140, 64, 24] },
    { width: 630, columns: [16, 'grow', 140, 140, 64, 24] },
    { width: 629, columns: [16, 'grow', 140, 64, 24] },
    { width: 478, columns: [16, 'grow', 140, 64, 24] },
    { width: 477, columns: [16, 'grow', 64, 24] },
  ])('a $width px table shows $columns', ({ width, columns }) => {
    expect(fitColumns(servers, width, 12).columns).toEqual(columns)
  })

  test('columns without a drop level stay when even they do not fit', () => {
    expect(fitColumns(servers, 200, 12).shown).toEqual({
      status: true,
      server: true,
      resources: false,
      cpu: false,
      memory: false,
      running: true,
      menu: true,
    })
  })

  test('a column set to undefined is not in the table', () => {
    // Live runners of one server: 816px inside the edges without the Server column.
    const runners = {
      state: 16,
      runner: 'grow',
      label: { width: 160, drop: 1 },
      server: undefined,
      job: { width: 300, drop: 3 },
      uptime: 72,
      menu: 24,
    } as const
    expect(fitColumns(runners, 818, 12)).toEqual({
      shown: { state: true, runner: true, label: true, server: false, job: true, uptime: true, menu: true },
      columns: [16, 'grow', 160, 300, 72, 24],
    })
    expect(fitColumns(runners, 817, 12).columns).toEqual([16, 'grow', 300, 72, 24])
  })

  test('columns of one level go together', () => {
    const labels = { label: 'grow', warm: { width: 80, drop: 1 }, idle: 48, starting: { width: 60, drop: 1 }, menu: 24 } as const
    expect(fitColumns(labels, 400, 12).shown).toEqual({ label: true, warm: false, idle: true, starting: false, menu: true })
  })
})
