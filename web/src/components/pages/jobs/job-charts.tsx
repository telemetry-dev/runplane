import { useQuery } from '@tanstack/react-query'
import { Link } from '@tanstack/react-router'
import type { ReactNode } from 'react'
import type { Schemas } from '~/api/client'
import { LineChart } from '~/components/app/line-chart'
import { formatMegabytes, formatPercent } from '~/lib/format'
import { clock } from './job-format'
import { metricsQuery } from './job-queries'
import { longestStep } from './job-steps'
import { TextButton } from './text-button'

/**
 * The CPU and Memory cards of Job detail: peak and average over the samples,
 * a line over the run with the longest step marked, and 0:00 to the duration
 * below. Samples come each 5 seconds, so a running job refetches them as often.
 * Under the cards, the size advice of the job's label.
 */
export function JobCharts({ job, now }: { job: Schemas['Job']; now: number }) {
  const running = job.state !== 'completed'
  const metrics = useQuery({ ...metricsQuery(job.id), refetchInterval: running ? 5000 : false })
  const samples = metrics.data?.samples ?? []

  const start = Date.parse(job.started_at ?? job.queued_at)
  const end = Math.max(start + 1000, job.finished_at ? Date.parse(job.finished_at) : now)
  const step = job.steps.length > 1 ? longestStep(job.steps, now) : undefined
  const stepStart = step?.started_at ? Date.parse(step.started_at) : start
  const axis = {
    domain: [start, end] as const,
    startLabel: clock(0),
    endLabel: clock(end - start),
    formatX: (x: number) => clock(x - start),
    marker: step && { x: stepStart, label: step.name },
  }

  const cpu = samples.map((sample) => ({ x: sample.t_ms, y: sample.cpu_percent }))
  const memory = samples.map((sample) => ({ x: sample.t_ms, y: sample.memory_mb }))
  const cpuPeak = job.cpu_peak ?? max(cpu)
  const cpuAverage = job.cpu_avg ?? average(cpu)
  const memoryPeak = job.mem_peak_mb ?? max(memory)
  const placeholder = metrics.isPending ? (
    <div aria-hidden className="h-[64px] animate-pulse rounded-s bg-neutral-3" />
  ) : metrics.isError ? (
    <p role="alert">
      Could not load the samples.{' '}
      <TextButton onClick={() => void metrics.refetch()}>Try again</TextButton>
    </p>
  ) : (
    <p>{running ? 'No samples yet.' : 'No samples for this job.'}</p>
  )
  const hasLine = samples.length > 1

  return (
    <div className="flex flex-col gap-m">
      <div className="flex flex-col gap-xl @min-[640px]:flex-row">
        <ChartCard
          title="CPU"
          value={cpuPeak === null ? '—' : `${formatPercent(cpuPeak)} peak`}
          detail={`${cpuAverage === null ? '' : `${formatPercent(cpuAverage)} avg `}of ${job.vcpu} vCPU`}
        >
          {hasLine ? (
            <LineChart aria-label="CPU use" valueName="CPU" points={cpu} max={100} formatY={formatPercent} {...axis} />
          ) : (
            <Placeholder>{placeholder}</Placeholder>
          )}
        </ChartCard>
        <ChartCard
          title="Memory"
          value={memoryPeak === null ? '—' : `${formatMegabytes(memoryPeak)} peak`}
          detail={`of ${formatMegabytes(job.memory_mb)}`}
        >
          {hasLine ? (
            <LineChart aria-label="Memory use" valueName="Memory" points={memory} max={job.memory_mb} limitLine formatY={formatMegabytes} {...axis} />
          ) : (
            <Placeholder>{placeholder}</Placeholder>
          )}
        </ChartCard>
      </div>
      <SizeAdvice advice={job.size_advice} />
    </div>
  )
}

/**
 * One quiet line when the last runs of this job need a larger label or fit a
 * smaller one: the API's reason, and the label that fits, with a link to it.
 */
function SizeAdvice({ advice }: { advice: Schemas['SizeAdvice'] }) {
  if (advice.kind === 'none') return null
  return (
    <p className="px-l text-s text-pretty text-neutral-7">
      {advice.reason}
      {advice.label && (
        <>
          {' '}
          <Link
            to="/runners"
            search={{ label: advice.label }}
            className="font-code text-xs tracking-m text-neutral-10 underline decoration-1 underline-offset-2"
          >
            {advice.label}
          </Link>
          {advice.kind === 'larger' ? ' has more room.' : ' is large enough.'}
        </>
      )}
    </p>
  )
}

function ChartCard({ title, value, detail, children }: { title: string; value: string; detail: string; children: ReactNode }) {
  return (
    <section className="flex min-w-0 grow basis-0 flex-col gap-m rounded-m bg-neutral-2 p-l">
      <div className="flex flex-wrap items-baseline justify-between gap-x-m">
        <h2 className="text-m font-heavy tracking-l text-neutral-10">{title}</h2>
        <p className="flex items-baseline gap-xs">
          <span className="text-s font-medium text-neutral-10">{value}</span>
          <span className="text-xs text-neutral-7">{detail}</span>
        </p>
      </div>
      {children}
    </section>
  )
}

/** Takes the height of a chart with its axis labels, so the cards keep their size. */
function Placeholder({ children }: { children: ReactNode }) {
  return <div className="flex h-[calc(64px+var(--space-m)+var(--line-xs))] flex-col justify-center text-s text-neutral-7">{children}</div>
}

function max(points: readonly { y: number }[]): number | null {
  return points.length === 0 ? null : Math.max(...points.map((point) => point.y))
}

function average(points: readonly { y: number }[]): number | null {
  return points.length === 0 ? null : points.reduce((sum, point) => sum + point.y, 0) / points.length
}
