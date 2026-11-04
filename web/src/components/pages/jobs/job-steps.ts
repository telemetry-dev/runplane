import type { Schemas } from '~/api/client'
import { stepStatus } from '~/components/app/status-icon'
import { stepTime } from './job-format'

type Step = Schemas['JobStep']

/**
 * The step whose log opens with the page: the first failed step, or 0 (the
 * whole log) when no step failed.
 */
export function defaultStep(steps: readonly Step[]): number {
  return steps.find((step) => stepStatus(step) === 'failed')?.number ?? 0
}

/**
 * The step whose log shows: the step in the URL, or the default step. Only a
 * stored log splits into steps. A live log is the sandbox's stream of the
 * whole job, so it shows whole (0).
 */
export function shownStep(job: Pick<Schemas['Job'], 'log_state' | 'steps'>, search: { step?: number }): number {
  if (job.log_state !== 'stored') return 0
  return search.step ?? defaultStep(job.steps)
}

/** The step that ran longest, for the marker on the CPU and Memory charts. */
export function longestStep(steps: readonly Step[], now: number): Step | undefined {
  let longest: Step | undefined
  let longestTime = 0
  for (const step of steps) {
    const time = stepTime(step, now) ?? 0
    if (time > longestTime) {
      longest = step
      longestTime = time
    }
  }
  return longest
}

// GitHub starts the log of each step with a folded "##[group]Run <command>"
// section that echoes the command and its shell. The step list already names
// the step, so a step log leaves that section out, as the Job detail design does.
const runGroup = /^(?:\S+Z )?##\[group\]Run /
const endGroup = /^(?:\S+Z )?##\[endgroup\]/

/** The log of one step without its "Run" sections. */
export function withoutRunGroups(log: string): string {
  const lines = log.split('\n')
  const kept: string[] = []
  let skipping = false
  for (const line of lines) {
    if (skipping) {
      if (endGroup.test(line)) skipping = false
      continue
    }
    if (runGroup.test(line)) {
      skipping = true
      continue
    }
    kept.push(line)
  }
  return kept.join('\n')
}

/** validateSearch of Job detail: `step` is a step number, 0 for the whole log, or absent for the default step. */
export function parseJobSearch(search: Record<string, unknown>): { step?: number } {
  const step = Number(search.step)
  return { step: search.step !== undefined && Number.isInteger(step) && step >= 0 ? step : undefined }
}
