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

/**
 * Formats a time relative to now: "just now", "12s ago", "2m ago", "in 25m",
 * then "Oct 2" after a week. `long` gives the words of the settings designs:
 * "25 min ago", "3 hours ago", "in 7 days", then "Oct 2, 2026" after 30 days.
 */
export function formatRelativeTime(time: Time, now = Date.now(), { long = false } = {}): string {
  const diff = now - toMillis(time)
  const distance = Math.abs(diff)
  if (distance < (long ? 30 * second : 5 * second)) return 'just now'
  if (long ? Math.round(distance / day) >= 30 : distance >= 7 * day) {
    return formatDate(time, { year: long || new Date(time).getFullYear() !== new Date(now).getFullYear() })
  }
  const amount = long ? longAmount(distance) : shortAmount(distance)
  return diff > 0 ? `${amount} ago` : `in ${amount}`
}

/**
 * formatRelativeTime for a time that has passed, as when a check ran. A `now`
 * that ticks slowly can lag behind a time that just arrived; that reads
 * "just now", not "in 21s".
 */
export function formatTimeAgo(time: Time, now = Date.now()): string {
  return formatRelativeTime(time, Math.max(now, toMillis(time)))
}

/** Formats a calendar date: "Oct 2, 2026", or "Oct 2" without the year. */
export function formatDate(time: Time, { year = true } = {}): string {
  return (year ? dateWithYear : dateWithoutYear).format(new Date(time))
}

/** Formats a date and time in local time: "Nov 4, 03:00", or "Nov 4, 2026, 03:00:12 GMT+1" in full, for a tooltip. */
export function formatDateTime(time: Time, { full = false } = {}): string {
  return (full ? fullDateTime : dateTime).format(new Date(time))
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
const dateTime = new Intl.DateTimeFormat('en-US', { month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit', hourCycle: 'h23' })
const fullDateTime = new Intl.DateTimeFormat('en-US', {
  month: 'short',
  day: 'numeric',
  year: 'numeric',
  hour: '2-digit',
  minute: '2-digit',
  second: '2-digit',
  hourCycle: 'h23',
  timeZoneName: 'short',
})
const byteUnits = ['B', 'KB', 'MB', 'GB', 'TB', 'PB']

function shortAmount(distance: number): string {
  if (distance < minute) return `${Math.floor(distance / second)}s`
  if (distance < hour) return `${Math.floor(distance / minute)}m`
  if (distance < day) return `${Math.floor(distance / hour)}h`
  return `${Math.floor(distance / day)}d`
}

// Rounded, so an invite made a moment ago expires "in 7 days", not "in 6 days".
function longAmount(distance: number): string {
  const minutes = Math.round(distance / minute)
  if (minutes < 60) return `${minutes} min`
  const hours = Math.round(distance / hour)
  if (hours < 24) return plural(hours, 'hour')
  return plural(Math.round(distance / day), 'day')
}

function plural(n: number, unit: string): string {
  return `${n} ${unit}${n === 1 ? '' : 's'}`
}

function pad(n: number): string {
  return String(n).padStart(2, '0')
}

function toMillis(time: Time): number {
  return typeof time === 'number' ? time : new Date(time).getTime()
}
