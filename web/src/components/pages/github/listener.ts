import type { Schemas } from '~/api/client'

type ScaleSet = Schemas['ScaleSetStatus']

export type ListenerSummary = {
  state: 'off' | 'starting' | 'connected' | 'failing'
  /** Scale sets that runplane keeps; ones it is deleting do not count. */
  total: number
  /** Scale sets whose listener reports an error. */
  failing: number
  /** The failing scale sets, grouped by error message in the order they first fail. */
  errors: Array<{ message: string; scaleSets: ScaleSet[] }>
  /** The latest poll of any listener, or null before the first one. */
  lastPoll: string | null
  /** The runner groups of the organization scale sets, as "Default". Personal scale sets belong to a repository instead. */
  runnerGroups: string[]
}

/**
 * Sums up the listeners of all scale sets for the GitHub settings: one failing
 * listener makes the whole connection "failing", because some jobs would wait.
 */
export function summarizeListeners(scaleSets: readonly ScaleSet[]): ListenerSummary {
  const kept = scaleSets.filter((set) => set.status !== 'deleting')
  const errors = new Map<string, ScaleSet[]>()
  let failing = 0
  let lastPoll: string | null = null
  const runnerGroups = new Set<string>()
  for (const set of kept) {
    if (!set.repository && set.runner_group) runnerGroups.add(set.runner_group)
    if (set.status === 'error') {
      failing++
      errors.set(set.error, [...(errors.get(set.error) ?? []), set])
    }
    if (set.last_poll_at && (!lastPoll || Date.parse(set.last_poll_at) > Date.parse(lastPoll))) lastPoll = set.last_poll_at
  }

  let state: ListenerSummary['state'] = 'connected'
  if (kept.length === 0) state = 'off'
  else if (failing > 0) state = 'failing'
  else if (kept.every((set) => set.status === 'creating')) state = 'starting'
  return {
    state,
    total: kept.length,
    failing,
    errors: [...errors].map(([message, sets]) => ({ message, scaleSets: sets })),
    lastPoll,
    runnerGroups: [...runnerGroups],
  }
}
