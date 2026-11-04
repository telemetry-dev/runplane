import { expect, test } from 'vitest'
import { safeRedirectPath } from './redirect'

test.each([
  ['/jobs?status=failed', '/jobs?status=failed'],
  ['/settings/github', '/settings/github'],
  [undefined, '/'],
  ['', '/'],
  ['https://evil.example/login', '/'],
  ['//evil.example', '/'],
  ['/\\evil.example', '/'],
  ['jobs', '/'],
])('safeRedirectPath(%j) is %j', (value, want) => {
  expect(safeRedirectPath(value)).toBe(want)
})
