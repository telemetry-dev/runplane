import { expect, test } from 'vitest'
import { lineNumber, listItems } from './list-text'

test('DNS servers split on commas and spaces', () => {
  expect(listItems('1.1.1.1, 8.8.8.8  9.9.9.9,', false)).toEqual(['1.1.1.1', '8.8.8.8', '9.9.9.9'])
  expect(listItems('  ', false)).toEqual([])
})

test('the allowlist has one entry on each line and skips blank lines', () => {
  expect(listItems('github.com\n\n  registry.npmjs.org \n10.0.0.0/8\n', true)).toEqual(['github.com', 'registry.npmjs.org', '10.0.0.0/8'])
})

test('the line of an allowlist item counts the blank lines that the list skips', () => {
  const text = 'github.com\n\nnot a host\n  \nregistry.npmjs.org'
  expect([0, 1, 2].map((index) => lineNumber(text, index))).toEqual([1, 3, 5])
})
