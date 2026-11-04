import { useLayoutEffect, useRef, useState, type RefObject } from 'react'
import type { Schemas } from '~/api/client'
import { BarChart } from '~/components/app/bar-chart'
import { ChartLegend } from '~/components/app/chart'
import { SectionHeader } from '~/components/app/section-header'
import { jobsChart, type Range } from './data'

const legend = [
  { label: 'Succeeded', color: 'var(--neutral-5)' },
  { label: 'Failed', color: 'var(--neutral-10)' },
]

const minPlot = 172

/** The "Jobs per hour" card: completed jobs per bucket of the range, failed ones on top. */
export function JobsChart({ range, buckets }: { range: Range; buckets: readonly Schemas['OverviewBucket'][] }) {
  const chart = jobsChart(range, buckets)
  const area = useRef<HTMLDivElement>(null)
  const height = usePlotHeight(area)

  return (
    <section className="flex min-w-0 flex-col gap-l rounded-m bg-neutral-2 p-l xl:grow-3 xl:basis-0">
      <SectionHeader title={chart.title} action={<ChartLegend items={legend} />} />
      {/* The area is as tall as the plot and axis need, or as the row stretches it; the chart fills it. */}
      <div ref={area} className="relative flex-1" style={{ minHeight: `calc(${minPlot}px + var(--space-s) + var(--line-xs))` }}>
        <div className="absolute inset-x-0 top-0">
          <BarChart aria-label={chart.title} bars={chart.bars} ticks={chart.ticks} endLabel="Now" valueName="Jobs" partName="Failed" height={height} />
        </div>
        {chart.empty && (
          <p className="pointer-events-none absolute inset-x-0 top-0 flex items-center justify-center text-s text-neutral-7" style={{ height }}>
            No completed jobs in {chart.period}
          </p>
        )}
      </div>
    </section>
  )
}

/**
 * Plot height that fills the area. Beside a long server list the row is taller
 * than the chart needs; the plot grows instead of leaving a gap under it. The
 * axis takes what the area's min-height adds to the smallest plot.
 */
function usePlotHeight(ref: RefObject<HTMLElement | null>): number {
  const [height, setHeight] = useState(minPlot)
  useLayoutEffect(() => {
    const element = ref.current
    if (!element) return
    const observer = new ResizeObserver(() => {
      const axis = parseFloat(getComputedStyle(element).minHeight) - minPlot
      setHeight(Math.max(minPlot, element.clientHeight - axis))
    })
    observer.observe(element)
    return () => observer.disconnect()
  }, [ref])
  return height
}
