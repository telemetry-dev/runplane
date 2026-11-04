import { useEffect, useState } from 'react'

// A request that stays open, as a followed log, must end when the page goes
// into the back/forward cache. The cached page keeps it open otherwise, and
// browsers allow only six connections to one host over HTTP/1.1, so a few
// cached pages stall the app. useLiveEvents does the same for the event stream.

/** Aborts `controller` on pagehide. Returns the function that stops listening. */
export function abortOnPageHide(controller: AbortController): () => void {
  const abort = () => controller.abort()
  addEventListener('pagehide', abort)
  return () => removeEventListener('pagehide', abort)
}

/**
 * A count that grows each time the page comes back from the back/forward cache.
 * Put it in the dependencies of the effect that opens the request, so the
 * request opens again.
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
