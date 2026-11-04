// Display formats of the designs. Numbers use en-US grouping ("1,284") in every
// browser locale so that tables read the same for all users.

type Time = string | number | Date

const second = 1_000
const minute = 60 * second
const hour = 60 * minute
const day = 24 * hour

/**
 * Formats a length of time: "48s", "1m 42s", "3m 05s", "2h 05m".
 * With `fraction`, times below 10 seconds keep one decimal ("2.8s").
 */
export function formatDuration(ms: number, { fraction = false } = {}): string {
  const clamped = Math.max(0, ms)
  if (fraction && clamped < 10 * second) return `${Number((clamped / second).toFixed(1))}s`

  const seconds = Math.floor(clamped / second)
  if (seconds < 60) return `${seconds}s`
  const minutes = Math.floor(seconds / 60)
  if (minutes < 60) return `${minutes}m ${pad(seconds % 60)}s`
  return `${Math.floor(minutes / 60)}h ${pad(minutes % 60)}m`
}

/** Formats a time relative to now: "now", "12s ago", "2m ago", "in 25m", then "Oct 2" after a week. */
export function formatRelativeTime(time: Time, now = Date.now()): string {
  const diff = now - toMillis(time)
  const distance = Math.abs(diff)
  if (distance < 5 * second) return 'now'
  if (distance >= 7 * day) return formatDate(time, { year: new Date(time).getFullYear() !== new Date(now).getFullYear() })

  const amount =
    distance < minute
      ? `${Math.floor(distance / second)}s`
      : distance < hour
        ? `${Math.floor(distance / minute)}m`
        : distance < day
          ? `${Math.floor(distance / hour)}h`
          : `${Math.floor(distance / day)}d`
  return diff > 0 ? `${amount} ago` : `in ${amount}`
}

/**
 * formatRelativeTime for a time that has passed, as when a check ran. A `now`
 * that ticks slowly can lag behind a time that just arrived; that reads "now",
 * not "in 21s".
 */
export function formatTimeAgo(time: Time, now = Date.now()): string {
  return formatRelativeTime(time, Math.max(now, toMillis(time)))
}

/** Formats a calendar date: "Oct 2, 2026", or "Oct 2" without the year. */
export function formatDate(time: Time, { year = true } = {}): string {
  return (year ? dateWithYear : dateWithoutYear).format(new Date(time))
}

export function formatCount(n: number): string {
  return count.format(n)
}

/** Formats a size in binary units, as df and free do: "512 B", "11.4 GB", "128 GB", "1.9 TB". */
export function formatBytes(bytes: number): string {
  let value = Math.max(0, bytes)
  let unit = 0
  while (value >= 1024 && unit < byteUnits.length - 1) {
    value /= 1024
    unit++
  }
  let rounded = value >= 100 ? Math.round(value) : Math.round(value * 10) / 10
  if (rounded >= 1024 && unit < byteUnits.length - 1) {
    rounded = 1
    unit++
  }
  return `${rounded} ${byteUnits[unit]}`
}

/** Formats a size the API reports in megabytes (memory_mb, mem_peak_mb). */
export function formatMegabytes(mb: number): string {
  return formatBytes(mb * 1024 * 1024)
}

/** Formats a value from 0 to 100: one decimal below 10 ("4.1%"), whole numbers above ("92%"). */
export function formatPercent(value: number): string {
  const rounded = Math.abs(value) < 10 ? Number(value.toFixed(1)) : Math.round(value)
  return `${rounded}%`
}

const count = new Intl.NumberFormat('en-US')
const dateWithYear = new Intl.DateTimeFormat('en-US', { month: 'short', day: 'numeric', year: 'numeric' })
const dateWithoutYear = new Intl.DateTimeFormat('en-US', { month: 'short', day: 'numeric' })
const byteUnits = ['B', 'KB', 'MB', 'GB', 'TB', 'PB']

function pad(n: number): string {
  return String(n).padStart(2, '0')
}

function toMillis(time: Time): number {
  return typeof time === 'number' ? time : new Date(time).getTime()
}
