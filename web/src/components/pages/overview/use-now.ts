import { useEffect, useState } from 'react'

/** The current time in milliseconds, updated each `intervalMs`, for times that count up on screen. */
export function useNow(intervalMs = 1000): number {
  const [now, setNow] = useState(Date.now)
  useEffect(() => {
    const timer = setInterval(() => setNow(Date.now()), intervalMs)
    return () => clearInterval(timer)
  }, [intervalMs])
  return now
}
