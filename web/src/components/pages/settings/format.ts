import { formatDate } from '~/lib/format'

// Date and time formats of the settings designs that ~/lib/format does not have.

const minute = 60_000
const hour = 60 * minute
const day = 24 * hour

/**
 * A time relative to now in words, as the Members design shows it: "just now",
 * "25 min ago", "3 hours ago", "in 7 days". After 30 days it shows the date.
 */
export function relativeTime(time: string | Date, now = Date.now()): string {
  const diff = new Date(time).getTime() - now
  const distance = Math.abs(diff)
  // Rounded, so an invite made a moment ago expires "in 7 days", not "in 6 days".
  const minutes = Math.round(distance / minute)
  const hours = Math.round(distance / hour)
  const days = Math.round(distance / day)
  if (minutes === 0) return 'just now'
  if (days >= 30) return formatDate(time)

  const amount = minutes < 60 ? `${minutes} min` : hours < 24 ? plural(hours, 'hour') : plural(days, 'day')
  return diff < 0 ? `${amount} ago` : `in ${amount}`
}

/** "Nov 4, 03:00" in local time. */
export function shortDateTime(time: string | Date): string {
  return dateTime.format(new Date(time))
}

/** "Nov 4, 2026, 03:00:12 GMT+1" in local time, for a tooltip. */
export function fullDateTime(time: string | Date): string {
  return full.format(new Date(time))
}

function plural(n: number, unit: string): string {
  return `${n} ${unit}${n === 1 ? '' : 's'}`
}

const dateTime = new Intl.DateTimeFormat('en-US', { month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit', hourCycle: 'h23' })
const full = new Intl.DateTimeFormat('en-US', {
  month: 'short',
  day: 'numeric',
  year: 'numeric',
  hour: '2-digit',
  minute: '2-digit',
  second: '2-digit',
  hourCycle: 'h23',
  timeZoneName: 'short',
})
