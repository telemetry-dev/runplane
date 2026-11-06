import { createFileRoute } from '@tanstack/react-router'
import { CachePage, CacheSkeleton, prefetch } from '~/components/pages/cache/cache-page'
import { pageHead } from '~/lib/head'

export const Route = createFileRoute('/_app/cache')({
  head: () => pageHead('Cache'),
  loader: ({ context: { queryClient } }) => prefetch(queryClient),
  pendingComponent: CacheSkeleton,
  pendingMs: 200,
  component: CachePage,
})
