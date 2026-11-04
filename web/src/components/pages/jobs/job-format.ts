import type { Schemas } from '~/api/client'

// Text and times of a job, shared by the Jobs table and Job detail.

type JobTimes = Pick<Schemas['JobSummary'], 'state' | 'queued_at' | 'started_at' | 'queue_ms' | 'duration_ms'>

/** Time in the queue. It grows while the job waits. */
export function queueTime(job: JobTimes, now: number): number {
  return job.state === 'queued' ? now - Date.parse(job.queued_at) : job.queue_ms
}

/** Time from start to finish, or null before the job starts. It grows while the job runs. */
export function runTime(job: JobTimes, now: number): number | null {
  if (!job.started_at) return null
  if (job.state === 'completed') return job.duration_ms
  return now - Date.parse(job.started_at)
}

/** Time of a step, or null before it starts. It grows while the step runs. */
export function stepTime(step: Schemas['JobStep'], now: number): number | null {
  if (!step.started_at) return null
  const end = step.completed_at ? Date.parse(step.completed_at) : now
  return end - Date.parse(step.started_at)
}

/** Why a job whose log_state is "none" has no log. */
export function noLogReason(job: Pick<Schemas['Job'], 'state' | 'started_at' | 'reject_reason'>): string {
  if (job.reject_reason) return `runplane does not run this job: ${job.reject_reason}`
  if (job.state !== 'completed') return 'The log starts when the job runs.'
  // The download of a finished job's log failed, or the job was cancelled in the queue.
  return job.started_at ? 'GitHub has no log for this job.' : 'The job ended before it ran.'
}

/** "ci.yml" from ".github/workflows/ci.yml". */
export function workflowFile(path: string): string {
  return path.slice(path.lastIndexOf('/') + 1)
}

/** "#4182", or nothing before runplane reads the run number from GitHub. */
export function runNumber(job: Pick<Schemas['JobSummary'], 'run_number'>): string {
  return job.run_number > 0 ? `#${job.run_number}` : ''
}

/** The line under a job name: "kestrel-hq/api · ci.yml · #4182". */
export function jobOrigin(job: Pick<Schemas['JobSummary'], 'repository' | 'workflow_file' | 'run_number'>): string {
  return [job.repository, workflowFile(job.workflow_file), runNumber(job)].filter(Boolean).join(' · ')
}

/** What started the run: "PR #412 by mira-k", "push by ada", "schedule". */
export function trigger(job: Pick<Schemas['Job'], 'event' | 'actor' | 'pr_number'>): string {
  const event = job.pr_number > 0 ? `PR #${job.pr_number}` : job.event.replaceAll('_', ' ')
  return job.actor ? `${event} by ${job.actor}` : event
}

/** A chart time as on a clock: "0:00", "6:48", "1:02:05". */
export function clock(ms: number): string {
  const seconds = Math.max(0, Math.floor(ms / 1000))
  const minutes = Math.floor(seconds / 60)
  const pad = (n: number) => String(n).padStart(2, '0')
  if (minutes < 60) return `${minutes}:${pad(seconds % 60)}`
  return `${Math.floor(minutes / 60)}:${pad(minutes % 60)}:${pad(seconds % 60)}`
}
