import { expect, test } from 'vitest'
import type { Schemas } from '~/api/client'
import { summarizeListeners } from './listener'

function scaleSet(fields: Partial<Schemas['ScaleSetStatus']>): Schemas['ScaleSetStatus'] {
  return {
    id: 'set',
    pool_id: 'pool',
    pool_label: 'runplane-4cpu',
    installation_id: 58213907,
    account_login: 'kestrel-hq',
    repository: '',
    runner_group: 'Default',
    status: 'listening',
    error: '',
    last_poll_at: '2026-11-04T04:49:33.447Z',
    stats: {
      available_jobs: 0,
      acquired_jobs: 0,
      assigned_jobs: 0,
      running_jobs: 0,
      registered_runners: 0,
      busy_runners: 0,
      idle_runners: 0,
    },
    ...fields,
  }
}

test('without scale sets the listener is off', () => {
  expect(summarizeListeners([])).toEqual({ state: 'off', total: 0, failing: 0, errors: [], lastPoll: null, runnerGroups: [] })
})

test('one failing listener marks the connection as failing, even next to listening ones', () => {
  const summary = summarizeListeners([
    scaleSet({ id: 'a' }),
    scaleSet({ id: 'broken', status: 'error', error: 'Bad credentials' }),
    scaleSet({ id: 'b', status: 'creating' }),
  ])
  expect(summary).toMatchObject({ state: 'failing', total: 3, failing: 1 })
})

test('failing scale sets are grouped by their error message', () => {
  const credentials = [
    scaleSet({ id: '2cpu', status: 'error', error: 'Bad credentials' }),
    scaleSet({ id: '8cpu', status: 'error', error: 'Bad credentials' }),
  ]
  const group = scaleSet({ id: 'arm', status: 'error', error: 'Runner group not found' })
  const summary = summarizeListeners([credentials[0]!, group, scaleSet({ id: 'ok' }), credentials[1]!])
  expect(summary.failing).toBe(3)
  expect(summary.errors).toEqual([
    { message: 'Bad credentials', scaleSets: credentials },
    { message: 'Runner group not found', scaleSets: [group] },
  ])
})

test('the connection is starting until a listener has made its scale set', () => {
  const sets = [scaleSet({ status: 'creating', last_poll_at: null }), scaleSet({ status: 'creating', last_poll_at: null })]
  expect(summarizeListeners(sets)).toMatchObject({ state: 'starting', lastPoll: null })
  expect(summarizeListeners([...sets, scaleSet({ status: 'listening' })]).state).toBe('connected')
})

test('scale sets that runplane deletes neither count nor fail the connection', () => {
  const summary = summarizeListeners([scaleSet({ status: 'deleting', error: 'gone' }), scaleSet({ status: 'listening' })])
  expect(summary).toMatchObject({ state: 'connected', total: 1, failing: 0, errors: [] })
  expect(summarizeListeners([scaleSet({ status: 'deleting' })]).state).toBe('off')
})

test('the last poll is the latest poll of any listener', () => {
  const summary = summarizeListeners([
    scaleSet({ last_poll_at: '2026-11-04T04:49:03.447Z' }),
    scaleSet({ last_poll_at: null }),
    scaleSet({ last_poll_at: '2026-11-04T04:49:40.447Z' }),
    scaleSet({ last_poll_at: '2026-11-04T04:49:27.447Z' }),
    scaleSet({ status: 'deleting', last_poll_at: '2026-11-04T05:00:00.000Z' }),
  ])
  expect(summary.lastPoll).toBe('2026-11-04T04:49:40.447Z')
})

test('runner groups come from the organization scale sets, each once', () => {
  const summary = summarizeListeners([
    scaleSet({ id: 'a' }),
    scaleSet({ id: 'b' }),
    scaleSet({ id: 'c', runner_group: 'Group 7' }),
    scaleSet({ id: 'personal', account_login: 'ada-okafor', repository: 'ada-okafor/notes', runner_group: 'Default' }),
  ])
  expect(summary.runnerGroups).toEqual(['Default', 'Group 7'])
  expect(summarizeListeners([scaleSet({ repository: 'ada-okafor/notes' })]).runnerGroups).toEqual([])
})
