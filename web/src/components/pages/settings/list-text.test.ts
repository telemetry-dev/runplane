import { expect, test } from 'vitest'
import { ApiError } from '~/api/client'
import { lineNumber, listItems } from './list-text'
import { saveError } from './save'

test('DNS servers split on commas and spaces', () => {
  expect(listItems('1.1.1.1, 8.8.8.8  9.9.9.9,', false)).toEqual(['1.1.1.1', '8.8.8.8', '9.9.9.9'])
  expect(listItems('  ', false)).toEqual([])
})

test('the allowlist has one entry on each line and skips blank lines', () => {
  expect(listItems('github.com\n\n  registry.npmjs.org \n10.0.0.0/8\n', true)).toEqual(['github.com', 'registry.npmjs.org', '10.0.0.0/8'])
})

test('an allowlist error names the line the user typed it on', () => {
  const text = 'github.com\n\nnot a host\n'
  const error = new ApiError(
    422,
    {
      type: 'about:blank',
      errors: [{ location: 'body.egress_allowlist[1]', message: 'Enter a host name, IPv4 address, or IPv4 CIDR.' }],
    },
    'Unprocessable Entity',
  )
  expect(saveError(error, (index, message) => `Line ${lineNumber(text, index)}: ${message}`)).toBe(
    'Line 3: Enter a host name, IPv4 address, or IPv4 CIDR.',
  )
})
