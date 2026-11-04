import type { QueryClient } from '@tanstack/react-query'
import { createRootRouteWithContext, HeadContent, Link, Outlet, Scripts, ScriptOnce } from '@tanstack/react-router'
import type { ReactNode } from 'react'
import { Toaster } from '~/components/ui/toast'
import { useThemeMode, themeModeScript } from '~/lib/theme-mode'
import appCss from '~/styles/app.css?url'

export const Route = createRootRouteWithContext<{ queryClient: QueryClient }>()({
  head: () => ({
    meta: [
      { charSet: 'utf-8' },
      { name: 'viewport', content: 'width=device-width, initial-scale=1' },
      { title: 'runplane' },
    ],
    links: [
      { rel: 'stylesheet', href: appCss },
      { rel: 'icon', href: '/favicon.svg', type: 'image/svg+xml' },
    ],
  }),
  shellComponent: RootDocument,
  component: Root,
  notFoundComponent: NotFound,
})

function RootDocument({ children }: { children: ReactNode }) {
  return (
    // The theme script adds the dark class before React hydrates.
    <html lang="en" suppressHydrationWarning>
      <head>
        <ScriptOnce>{themeModeScript}</ScriptOnce>
        <HeadContent />
      </head>
      <body>
        {children}
        <Scripts />
      </body>
    </html>
  )
}

function Root() {
  useThemeMode()
  return (
    <>
      <Outlet />
      <Toaster />
    </>
  )
}

function NotFound() {
  return (
    <main className="flex min-h-dvh flex-col items-center justify-center gap-m">
      <h1 className="type-page-title text-neutral-10">Page not found</h1>
      <Link to="/" className="text-s text-neutral-8 underline underline-offset-2 hover:text-neutral-10">
        Go to the overview
      </Link>
    </main>
  )
}
