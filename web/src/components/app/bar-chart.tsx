import { useState, type CSSProperties } from 'react'
import { cx } from '~/lib/cx'
import { barScale, ChartTooltip, keyIndex } from './chart'

export type BarChartBar = {
  /** The bucket, as "11:00 – 12:00". Titles the tooltip and the table row. */
  label: string
  /** Height of the gray bar, for example all jobs of the hour. */
  value: number
  /** The black cap at the top of the bar, a part of `value`, for example the failed jobs. */
  part: number
  /** Second tooltip line, as "131 jobs · 6 failed". */
  detail: string
}

type BarChartProps = {
  bars: readonly BarChartBar[]
  /** Labels under the bars, each left-aligned with the bar at `at`. */
  ticks: ReadonlyArray<{ at: number; label: string }>
  /** Label at the right end of the axis, as "Now". */
  endLabel?: string
  /** Column names of the table that screen readers get. */
  valueName: string
  partName: string
  'aria-label': string
  /** Plot height in pixels. */
  height?: number
}

/**
 * "Jobs per hour" of Overview: gray bars with black caps, dashed gridlines, and
 * a black tooltip on the hovered or focused column. Arrow keys move between
 * columns. Put a ChartLegend for the two series next to the chart title.
 */
export function BarChart({ bars, ticks, endLabel, valueName, partName, height = 172, ...props }: BarChartProps) {
  const [active, setActive] = useState<number | null>(null)
  const { top, lines } = barScale(Math.max(0, ...bars.map((bar) => bar.value)))
  const count = bars.length
  const focused = active === null ? undefined : bars[active]

  return (
    <figure className="flex flex-col gap-s">
      <div
        role="group"
        aria-label={props['aria-label']}
        tabIndex={0}
        className="relative outline-none focus-visible:outline-2 focus-visible:outline-offset-2"
        style={{ height }}
        onPointerLeave={() => setActive(null)}
        onBlur={() => setActive(null)}
        onKeyDown={(event) => {
          const next = keyIndex(event.key, active, count)
          if (next === undefined) return
          event.preventDefault()
          setActive(next)
        }}
      >
        {lines.map((value) => (
          <div
            key={value}
            aria-hidden
            className="absolute inset-x-zero h-px bg-[repeating-linear-gradient(to_right,var(--neutral-4)_0_2px,transparent_2px_6px)]"
            style={{ bottom: `${(value / top) * 100}%` }}
          />
        ))}
        <div className="absolute inset-zero flex" aria-hidden>
          {bars.map((bar, index) => (
            <div
              key={index}
              className={cx('flex h-full min-w-0 flex-1 items-end justify-center rounded-s', index === active && 'bg-neutral-3')}
              onPointerEnter={() => setActive(index)}
            >
              {bar.value > 0 && (
                <div
                  className="relative w-[min(16px,62%)] overflow-hidden rounded-xs bg-neutral-5"
                  style={{ height: `max(2px, ${(bar.value / top) * 100}%)` }}
                >
                  {bar.part > 0 && (
                    <div
                      className="absolute inset-x-zero top-zero rounded-xs bg-neutral-10"
                      style={{ height: `max(4px, ${(bar.part / bar.value) * 100}%)` }}
                    />
                  )}
                </div>
              )}
            </div>
          ))}
        </div>
        {focused && active !== null && <ChartTooltip title={focused.label} detail={focused.detail} style={tooltipPosition(active, count)} />}
        <p className="sr-only" aria-live="polite">
          {focused ? `${focused.label}: ${focused.detail}` : ''}
        </p>
      </div>
      <div className="relative flex h-(--line-xs) text-xs text-neutral-7 tabular-nums" aria-hidden>
        {bars.map((_, index) => {
          const tick = ticks.find((candidate) => candidate.at === index)
          return (
            <div key={index} className="relative min-w-0 flex-1">
              {/* Starts at the bar's left edge: the bar is centered and min(16px, 62%) wide. */}
              {tick && <span className="absolute left-[calc(50%-min(8px,31%))] whitespace-nowrap">{tick.label}</span>}
            </div>
          )
        })}
        {endLabel && <span className="absolute right-zero">{endLabel}</span>}
      </div>
      <div className="sr-only">
        <table>
          <caption>{props['aria-label']}</caption>
          <thead>
            <tr>
              <th scope="col">Time</th>
              <th scope="col">{valueName}</th>
              <th scope="col">{partName}</th>
            </tr>
          </thead>
          <tbody>
            {bars.map((bar, index) => (
              <tr key={index}>
                <th scope="row">{bar.label}</th>
                <td>{bar.value}</td>
                <td>{bar.part}</td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </figure>
  )
}

// Beside the column, on the side with more room, L spacing away.
function tooltipPosition(index: number, count: number): CSSProperties {
  if (index + 0.5 > count / 2) return { top: 2, right: `calc(${(1 - index / count) * 100}% + var(--space-l))` }
  return { top: 2, left: `calc(${((index + 1) / count) * 100}% + var(--space-l))` }
}
