import { useLayoutEffect, useRef, useState } from 'react'
import { ChartTooltip, keyIndex, nearestIndex, useElementWidth } from './chart'

export type LinePoint = { x: number; y: number }

type LineChartProps = {
  /** Samples sorted by x, for example { x: unix ms, y: CPU percent }. */
  points: readonly LinePoint[]
  /** Top of the y scale: 100 for percent, the memory limit for memory. */
  max: number
  /** Draws the dashed line at `max`, as the memory limit. */
  limitLine?: boolean
  /** The x range, for example the job's start and end. Defaults to the first and last sample. */
  domain?: readonly [number, number]
  /** A dashed vertical line with a label under it, as the start of the "Run Playwright" step. */
  marker?: { x: number; label: string }
  /** Axis labels at both ends, as "0:00" and "6:48". */
  startLabel: string
  endLabel: string
  formatX: (x: number) => string
  formatY: (y: number) => string
  /** Column name of the value in the table that screen readers get. */
  valueName: string
  'aria-label': string
  height?: number
}

/**
 * CPU and Memory charts of Job detail: a thin color-1 line over a baseline, with
 * a crosshair and tooltip that follow the pointer or the arrow keys.
 */
export function LineChart({
  points,
  max,
  limitLine = false,
  domain,
  marker,
  startLabel,
  endLabel,
  formatX,
  formatY,
  valueName,
  height = 64,
  ...props
}: LineChartProps) {
  const plot = useRef<HTMLDivElement>(null)
  const width = useElementWidth(plot)
  const [active, setActive] = useState<number | null>(null)
  const startLabelRef = useRef<HTMLSpanElement>(null)
  const [startWidth, setStartLabelWidth] = useState(0)
  useLayoutEffect(() => setStartLabelWidth(startLabelRef.current?.offsetWidth ?? 0), [startLabel])

  const [start, end] = domain ?? [points[0]?.x ?? 0, points.at(-1)?.x ?? 1]
  const span = end - start || 1
  const xOf = (x: number) => ((x - start) / span) * width
  // One pixel of room at the top and bottom keeps the 1.5px line inside the plot.
  const yOf = (y: number) => 1 + (1 - Math.min(1, Math.max(0, y / max))) * (height - 2)
  const xs = points.map((point) => xOf(point.x))
  const focused = active === null ? undefined : points[active]
  const markerX = marker ? xOf(marker.x) : undefined

  return (
    <figure className="flex flex-col gap-m">
      <div
        ref={plot}
        role="group"
        aria-label={props['aria-label']}
        tabIndex={0}
        className="relative outline-none focus-visible:outline-2 focus-visible:outline-offset-2"
        style={{ height }}
        onPointerMove={(event) => {
          if (points.length === 0) return
          setActive(nearestIndex(xs, event.clientX - event.currentTarget.getBoundingClientRect().left))
        }}
        onPointerLeave={() => setActive(null)}
        onBlur={() => setActive(null)}
        onKeyDown={(event) => {
          const next = keyIndex(event.key, active, points.length)
          if (next === undefined) return
          event.preventDefault()
          setActive(next)
        }}
      >
        {width > 0 && (
          <svg width={width} height={height} className="block overflow-visible" aria-hidden>
            <line x1={0} x2={width} y1={height - 0.5} y2={height - 0.5} stroke="var(--neutral-4)" />
            {limitLine && <line x1={0} x2={width} y1={0.5} y2={0.5} stroke="var(--neutral-6)" strokeDasharray="3 3" />}
            {markerX !== undefined && (
              <line x1={crisp(markerX)} x2={crisp(markerX)} y1={0} y2={height} stroke="var(--neutral-5)" strokeDasharray="2 3" />
            )}
            <polyline
              points={points.map((point, index) => `${xs[index]!.toFixed(1)},${yOf(point.y).toFixed(1)}`).join(' ')}
              fill="none"
              stroke="var(--color-1)"
              strokeWidth={1.5}
              strokeLinejoin="round"
              strokeLinecap="round"
            />
            {focused && active !== null && (
              <>
                <line x1={crisp(xs[active]!)} x2={crisp(xs[active]!)} y1={0} y2={height} stroke="var(--neutral-6)" />
                <circle cx={xs[active]} cy={yOf(focused.y)} r={3.5} fill="var(--color-1)" stroke="var(--neutral-2)" strokeWidth={2} />
              </>
            )}
          </svg>
        )}
        {focused && active !== null && (
          <ChartTooltip
            title={formatY(focused.y)}
            detail={formatX(focused.x)}
            style={xs[active]! > width / 2 ? { top: 0, right: `calc(${width - xs[active]!}px + var(--space-s))` } : { top: 0, left: `calc(${xs[active]}px + var(--space-s))` }}
          />
        )}
        <p className="sr-only" aria-live="polite">
          {focused ? `${formatX(focused.x)}: ${formatY(focused.y)}` : ''}
        </p>
      </div>
      <div className="relative flex h-(--line-xs) justify-between text-xs text-neutral-7 tabular-nums" aria-hidden>
        <span ref={startLabelRef}>{startLabel}</span>
        {marker && markerX !== undefined && (
          // Next to its line, but never over the start label when the marker is near 0.
          <span
            className="absolute truncate"
            style={{
              left: `max(${markerX}px + var(--space-xs), ${startWidth}px + var(--space-s))`,
              maxWidth: `calc(100% - max(${markerX}px, ${startWidth}px) - 6ch)`,
            }}
          >
            {marker.label}
          </span>
        )}
        <span>{endLabel}</span>
      </div>
      <div className="sr-only">
        <table>
          <caption>{props['aria-label']}</caption>
          <thead>
            <tr>
              <th scope="col">Time</th>
              <th scope="col">{valueName}</th>
            </tr>
          </thead>
          <tbody>
            {points.map((point) => (
              <tr key={point.x}>
                <th scope="row">{formatX(point.x)}</th>
                <td>{formatY(point.y)}</td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </figure>
  )
}

// A 1px vertical line sits on a pixel center, so it renders sharp instead of as two half-tone pixels.
function crisp(x: number): number {
  return Math.round(x) + 0.5
}
