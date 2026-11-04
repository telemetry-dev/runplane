import { expect, test } from 'vitest'
import { eventsSummary } from './channel-info'

test('channel events read as one sentence in the order of the channel', () => {
  expect(eventsSummary(['server.unreachable', 'backup.failed', 'runner.deprecation'])).toBe(
    'Server unreachable, backup failed, runner version end date',
  )
  expect(eventsSummary([])).toBe('')
})
