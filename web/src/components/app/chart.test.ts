import { describe, expect, test } from 'vitest'
import { barScale, keyIndex, nearestIndex } from './chart'

describe('barScale', () => {
  test.each([
    [6, [2, 4, 6]],
    [9, [2.5, 5, 7.5]],
    // A third of the top is 3.6, which is nearer to 5 than to 2.5 on a log scale.
    [10, [5, 10]],
    [140, [50, 100, 150]],
    [1600, [500, 1000, 1500]],
  ])('bars up to %d get gridlines at %j, under the top', (max, lines) => {
    const scale = barScale(max)
    expect(scale.lines).toEqual(lines)
    expect(scale.top).toBeGreaterThan(Math.max(max, ...lines))
  })

  test('has no gridlines without data, and a top that the bar heights can divide by', () => {
    const scale = barScale(0)
    expect(scale.lines).toEqual([])
    expect(scale.top).toBeGreaterThan(0)
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
