import { useLayoutEffect, useState, type CSSProperties, type ReactNode, type RefObject } from 'react'

// Pieces shared by BarChart and LineChart.

/** The black hover label of the charts, as "11:00 – 12:00 / 131 jobs · 6 failed". */
export function ChartTooltip({ title, detail, style }: { title: ReactNode; detail: ReactNode; style?: CSSProperties }) {
  return (
    <div
      aria-hidden
      className="pointer-events-none absolute z-10 flex flex-col gap-xxs rounded-s bg-neutral-10 px-m py-s whitespace-nowrap"
      style={style}
    >
      <span className="text-xs font-medium text-neutral-1">{title}</span>
      <span className="text-xs text-neutral-5">{detail}</span>
    </div>
  )
}

type LegendItem = { label: string; color: string }

/** Legend of a chart with two or more series: a swatch of each series and its name. */
export function ChartLegend({ items }: { items: readonly LegendItem[] }) {
  return (
    <ul className="flex flex-wrap items-center gap-m">
      {items.map((item) => (
        <li key={item.label} className="flex items-center gap-xs text-xs font-medium text-neutral-7">
          <span aria-hidden className="size-[8px] rounded-xs" style={{ backgroundColor: item.color }} />
          {item.label}
        </li>
      ))}
    </ul>
  )
}

/** The width of an element, kept up to date as it resizes. 0 until the first layout. */
export function useElementWidth(ref: RefObject<HTMLElement | null>): number {
  const [width, setWidth] = useState(0)
  useLayoutEffect(() => {
    const element = ref.current
    if (!element) return
    setWidth(element.clientWidth)
    const observer = new ResizeObserver(([entry]) => setWidth(entry!.contentRect.width))
    observer.observe(element)
    return () => observer.disconnect()
  }, [ref])
  return width
}

/** The round number (1, 2, 2.5, or 5 times a power of ten) closest to `rough`. */
export function niceStep(rough: number): number {
  if (!(rough > 0)) return 1
  const power = 10 ** Math.floor(Math.log10(rough))
  const candidates = [1, 2, 2.5, 5, 10].map((factor) => factor * power)
  // Closest on a log scale, so 2 and 5 are equally far from 3.16.
  return candidates.reduce((best, step) => (Math.abs(Math.log(step / rough)) < Math.abs(Math.log(best / rough)) ? step : best))
}

/**
 * The top of the value scale and the gridline values for bars up to `max`. The
 * top leaves headroom above the tallest bar; about three gridlines sit below it.
 */
export function barScale(max: number): { top: number; lines: number[] } {
  if (max <= 0) return { top: 1, lines: [] }
  const top = max * 1.08
  const step = niceStep(top / 3)
  const lines: number[] = []
  for (let value = step; value < top; value += step) lines.push(value)
  return { top, lines }
}

/** Index of the item whose x is closest to `x`. `xs` must be sorted. */
export function nearestIndex(xs: readonly number[], x: number): number {
  let low = 0
  let high = xs.length - 1
  while (high - low > 1) {
    const middle = (low + high) >> 1
    if (xs[middle]! < x) low = middle
    else high = middle
  }
  return Math.abs(xs[high]! - x) < Math.abs(xs[low]! - x) ? high : low
}

/** The next index for arrow keys and Home/End on a chart, or `undefined` for other keys. */
export function keyIndex(key: string, current: number | null, count: number): number | undefined {
  if (count === 0) return undefined
  if (key === 'Home') return 0
  if (key === 'End') return count - 1
  if (key === 'ArrowRight') return current === null ? 0 : Math.min(count - 1, current + 1)
  if (key === 'ArrowLeft') return current === null ? count - 1 : Math.max(0, current - 1)
  return undefined
}
