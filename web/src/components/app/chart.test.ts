import { describe, expect, test } from 'vitest'
import { barScale, keyIndex, nearestIndex, niceStep } from './chart'

describe('niceStep', () => {
  test.each([
    [48, 50],
    [3.2, 2.5],
    [0.7, 0.5],
    [140, 100],
    [1700, 2000],
  ])('rounds %d to %d', (rough, step) => {
    expect(niceStep(rough)).toBe(step)
  })

  test('falls back to 1 for no range', () => {
    expect(niceStep(0)).toBe(1)
  })
})

describe('barScale', () => {
  test('leaves headroom over the tallest bar and puts gridlines on round values below the top', () => {
    expect(barScale(140)).toEqual({ top: 140 * 1.08, lines: [50, 100, 150] })
    expect(barScale(6).lines).toEqual([2, 4, 6])
  })

  test('has no gridlines without data', () => {
    expect(barScale(0)).toEqual({ top: 1, lines: [] })
  })
})

describe('nearestIndex', () => {
  test('picks the closest sample on either side', () => {
    expect(nearestIndex([0, 10, 20], 14)).toBe(1)
    expect(nearestIndex([0, 10, 20], 16)).toBe(2)
    expect(nearestIndex([0, 10, 20], -5)).toBe(0)
    expect(nearestIndex([5], 100)).toBe(0)
  })
})

describe('keyIndex', () => {
  test('moves with the arrow keys and stops at the ends', () => {
    expect(keyIndex('ArrowRight', null, 3)).toBe(0)
    expect(keyIndex('ArrowLeft', null, 3)).toBe(2)
    expect(keyIndex('ArrowRight', 2, 3)).toBe(2)
    expect(keyIndex('ArrowLeft', 0, 3)).toBe(0)
    expect(keyIndex('End', 0, 3)).toBe(2)
    expect(keyIndex('Home', 2, 3)).toBe(0)
  })

  test('ignores other keys and empty charts', () => {
    expect(keyIndex('Enter', 1, 3)).toBeUndefined()
    expect(keyIndex('ArrowRight', null, 0)).toBeUndefined()
  })
})
