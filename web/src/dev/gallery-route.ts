import { createRoute, lazyRouteComponent, type AnyRoute } from '@tanstack/react-router'
import { pageHead } from '~/lib/head'

const path = '/dev/components'

/**
 * Adds the component gallery at /dev/components to the routes, for design
 * reviews in `pnpm dev`. The router calls it only in dev, so the production
 * build has neither the route nor the gallery.
 */
export function withGallery<T extends AnyRoute>(tree: T): T {
  // A router of each server request, and of each hot reload, gets the same tree.
  if (tree.children?.some((route: AnyRoute) => route.fullPath === path)) return tree
  const gallery = createRoute({
    getParentRoute: () => tree,
    path,
    head: () => pageHead('Components'),
    component: lazyRouteComponent(() => import('./gallery'), 'ComponentGallery'),
  })
  return tree.addChildren([...(tree.children ?? []), gallery]) as T
}
