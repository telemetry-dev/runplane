import { Link } from '@tanstack/react-router'
import type { Schemas } from '~/api/client'
import { StatusIcon } from '~/components/app/status-icon'
import { cx } from '~/lib/cx'
import { formatDuration } from '~/lib/format'
import { stepTime } from './job-format'

type JobStepsListProps = {
  job: Pick<Schemas['Job'], 'id' | 'steps' | 'log_state'>
  /** Number of the step whose log shows, 0 for the whole log. */
  selected: number
  now: number
}

/**
 * The Steps column of Job detail. A step links to its log with ?step=N; the
 * selected step links back to the whole log. Steps that did not run have no log
 * and are not links.
 */
export function JobStepsList({ job, selected, now }: JobStepsListProps) {
  const { steps } = job
  // Only a stored log splits into steps.
  const linked = job.log_state === 'stored'
  return (
    <section aria-labelledby="job-steps" className="flex w-full shrink-0 flex-col gap-s xl:w-[280px]">
      <h2 id="job-steps" className="px-s text-m font-heavy text-neutral-10">
        Steps
      </h2>
      {steps.length === 0 ? (
        <p className="px-s text-s text-neutral-7">Steps show when the job starts.</p>
      ) : (
        <ol className="flex flex-col gap-xxs">
          {steps.map((step) => (
            <li key={step.number}>
              <StepRow jobId={job.id} step={step} selected={step.number === selected} linked={linked} now={now} />
            </li>
          ))}
        </ol>
      )}
    </section>
  )
}

type StepRowProps = { jobId: string; step: Schemas['JobStep']; selected: boolean; linked: boolean; now: number }

function StepRow({ jobId, step, selected, linked, now }: StepRowProps) {
  const skipped = step.status === 'skipped'
  const time = stepTime(step, now)
  const row = 'flex items-center gap-s rounded-s px-s py-xs text-s'
  const content = (
    <>
      <StatusIcon status={step.status} size={14} />
      <span className={cx('min-w-0 grow truncate', selected ? 'font-medium text-neutral-10' : skipped ? 'text-neutral-6' : 'text-neutral-8')}>
        {step.name}
      </span>
      <span className={cx('shrink-0 tabular-nums', skipped ? 'text-neutral-6' : 'text-neutral-7')}>
        {skipped ? 'Skipped' : time !== null && formatDuration(time)}
      </span>
    </>
  )

  if (!linked || skipped || !step.started_at) return <div className={row}>{content}</div>
  return (
    <Link
      to="/jobs/$jobId"
      params={{ jobId }}
      search={{ step: selected ? 0 : step.number }}
      replace
      resetScroll={false}
      aria-current={selected || undefined}
      className={cx(row, 'transition-colors', selected ? 'bg-neutral-3' : 'hover:bg-neutral-2')}
    >
      {content}
    </Link>
  )
}
