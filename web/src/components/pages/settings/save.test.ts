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
