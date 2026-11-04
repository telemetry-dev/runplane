import { createFileRoute } from '@tanstack/react-router'
import { CachePage, CacheSkeleton } from '~/components/pages/cache/cache-page'
import { cacheQuery } from '~/components/pages/cache/queries'
import { pageHead } from '~/lib/head'

export const Route = createFileRoute('/_app/cache')({
  head: () => pageHead('Cache'),
  loader: ({ context: { queryClient } }) => queryClient.ensureQueryData(cacheQuery),
  pendingComponent: CacheSkeleton,
  pendingMs: 200,
  component: CachePage,
})
