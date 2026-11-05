import { createFileRoute } from '@tanstack/react-router'
import { cachesQuery } from '~/api/caches'
import { CachePage, CacheSkeleton } from '~/components/pages/cache/cache-page'
import { pageHead } from '~/lib/head'

export const Route = createFileRoute('/_app/cache')({
  head: () => pageHead('Cache'),
  loader: ({ context: { queryClient } }) => queryClient.ensureQueryData(cachesQuery),
  pendingComponent: CacheSkeleton,
  pendingMs: 200,
  component: CachePage,
})
