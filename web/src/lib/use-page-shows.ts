import { useEffect, useState } from 'react'

/**
 * A count that grows each time the page comes back from the back/forward
 * cache, for effects that must run again then.
 */
export function usePageShows(): number {
  const [shows, setShows] = useState(0)
  useEffect(() => {
    const onShow = (event: PageTransitionEvent) => {
      if (event.persisted) setShows((count) => count + 1)
    }
    addEventListener('pageshow', onShow)
    return () => removeEventListener('pageshow', onShow)
  }, [])
  return shows
}
