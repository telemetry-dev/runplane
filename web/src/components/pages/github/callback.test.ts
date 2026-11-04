import { expect, test } from 'vitest'
import { callbackSearch } from './callback'

test('callbackSearch keeps the error text and the requested flag of the GitHub callbacks', () => {
  expect(callbackSearch({ requested: 1 })).toEqual({ error: undefined, requested: true })
  expect(callbackSearch({ requested: '1' })).toEqual({ error: undefined, requested: true })
  expect(callbackSearch({ error: 'The code expired.' })).toEqual({ error: 'The code expired.', requested: undefined })
  expect(callbackSearch({ requested: 0, error: 42 })).toEqual({ error: undefined, requested: undefined })
  expect(callbackSearch({})).toEqual({ error: undefined, requested: undefined })
})
