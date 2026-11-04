import { useQueryErrorResetBoundary } from '@tanstack/react-query'
import { useRouter, type ErrorComponentProps } from '@tanstack/react-router'
import { createContext, useContext } from 'react'
import { errorMessage } from '~/api/client'
import { Button } from './ui/button'

/**
 * Set to true around the Outlet of a layout that shows the page title itself,
 * as Settings does; then a failed child shows a section heading, not a second h1.
 */
export const NestedRouteError = createContext(false)

/** Shown in place of a route that failed to load; the app shell around it stays. */
export function RouteError({ error }: ErrorComponentProps) {
  const router = useRouter()
  const queryReset = useQueryErrorResetBoundary()
  const nested = useContext(NestedRouteError)
  const Heading = nested ? 'h2' : 'h1'

  return (
    <div role="alert" className="flex flex-col items-start gap-m">
      <Heading className={nested ? 'type-section text-neutral-10' : 'type-page-title text-neutral-10'}>Something went wrong</Heading>
      <p className="text-s text-neutral-8">{errorMessage(error)}</p>
      <Button
        onClick={() => {
          queryReset.reset()
          void router.invalidate()
        }}
      >
        Try again
      </Button>
    </div>
  )
}
