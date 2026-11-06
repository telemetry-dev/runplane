import { expect, test } from 'vitest'
import { ApiError } from '~/api/client'
import { saveError } from './save'

test('a save error without field problems shows its detail; repeated messages show once', () => {
  expect(saveError(new ApiError(403, { type: 'about:blank', detail: 'Only admins can do this.' }, 'Forbidden'))).toBe(
    'Only admins can do this.',
  )
  const twice = new ApiError(
    422,
    {
      type: 'about:blank',
      errors: [
        { location: 'body.dns_servers[0]', message: 'Enter an IPv4 address.' },
        { location: 'body.dns_servers[2]', message: 'Enter an IPv4 address.' },
      ],
    },
    'Unprocessable Entity',
  )
  expect(saveError(twice)).toBe('Enter an IPv4 address.')
  expect(saveError(null)).toBeUndefined()
})

test('a save error gives the index of a list item to line, so the text can name the item', () => {
  const error = new ApiError(
    422,
    {
      type: 'about:blank',
      errors: [
        { location: 'body.egress_allowlist[1]', message: 'Enter a host name, IPv4 address, or IPv4 CIDR.' },
        { location: 'body.egress_allowlist', message: 'Enter at most 100 entries.' },
      ],
    },
    'Unprocessable Entity',
  )
  expect(saveError(error, (index, message) => `Item ${index}: ${message}`)).toBe(
    'Item 1: Enter a host name, IPv4 address, or IPv4 CIDR. Enter at most 100 entries.',
  )
})
