import { Dialog } from '@base-ui/react/dialog'
import { useQuery, type QueryClient } from '@tanstack/react-query'
import { Link, useLocation, type LinkProps } from '@tanstack/react-router'
import { Activity, Database, LayoutDashboard, Menu as MenuIcon, Server, Settings2, type LucideIcon } from 'lucide-react'
import { useEffect, useState, type ReactNode } from 'react'
import { githubQuery } from '~/api/github'
import { runningJobsQuery } from '~/api/jobs'
import { statusQuery } from '~/api/session'
import { LogoMark } from '~/components/ui/brand'
import { IconButton } from '~/components/ui/button'
import { modalBackdrop } from '~/components/ui/dialog'
import { CpuIcon, GithubIcon } from '~/components/ui/icons'
import { formatCount } from '~/lib/format'
import { InstanceMenu } from './instance-menu'
import { useFrameSubscribed } from './pending'
import { UserMenu } from './user-menu'

/** Starts the requests of the sidebar next to the page's, so a pending frame can show their data. */
export function prefetchSidebar(client: QueryClient) {
  void client.prefetchQuery({ ...githubQuery, staleTime: Infinity })
  void client.prefetchQuery({ ...runningJobsQuery, staleTime: Infinity })
}

/**
 * The 232px sidebar of the app designs: instance menu, navigation, GitHub account,
 * user menu. Below the md breakpoint it moves into a drawer behind a top bar.
 */
export function AppSidebar() {
  return (
    <>
      <aside className="sticky top-zero hidden h-dvh w-[232px] shrink-0 flex-col overflow-y-auto bg-neutral-2 md:flex">
        <SidebarContent />
      </aside>
      <MobileBar />
    </>
  )
}

function SidebarContent() {
  return (
    <div className="flex flex-1 flex-col gap-xl px-m py-l">
      <InstanceMenu />
      <nav aria-label="Main" className="flex flex-col gap-xxs">
        <NavItem to="/" icon={LayoutDashboard} exact>
          Overview
        </NavItem>
        <NavItem to="/jobs" icon={Activity} badge={<RunningJobsBadge />}>
          Jobs
        </NavItem>
        <NavItem to="/servers" icon={Server}>
          Servers
        </NavItem>
        <NavItem to="/runners" icon={CpuIcon}>
          Runners
        </NavItem>
        <NavItem to="/cache" icon={Database}>
          Cache
        </NavItem>
        <NavItem to="/settings" icon={Settings2}>
          Settings
        </NavItem>
      </nav>
      <div className="mt-auto flex flex-col gap-m">
        <GithubAccount />
        <UserMenu />
      </div>
    </div>
  )
}

function MobileBar() {
  const [open, setOpen] = useState(false)
  const pathname = useLocation({ select: (location) => location.pathname })
  const name = useQuery({ ...statusQuery, subscribed: useFrameSubscribed() }).data?.instance_name || 'runplane'

  // Close the drawer after a navigation link opens a page.
  useEffect(() => setOpen(false), [pathname])

  return (
    <header className="sticky top-zero z-10 flex items-center gap-s bg-neutral-2 px-l py-s md:hidden">
      <Dialog.Root open={open} onOpenChange={setOpen}>
        <Dialog.Trigger render={<IconButton label="Open navigation" className="-m-xxs p-xxs text-neutral-8" />}>
          <MenuIcon aria-hidden />
        </Dialog.Trigger>
        <Dialog.Portal>
          <Dialog.Backdrop className={modalBackdrop} />
          <Dialog.Popup
            aria-label="Navigation"
            className="fixed inset-y-zero left-zero z-20 flex w-[232px] max-w-[85vw] flex-col overflow-y-auto bg-neutral-2 transition-transform duration-(--motion-large-duration) ease-large data-starting-style:-translate-x-full data-ending-style:-translate-x-full"
          >
            <SidebarContent />
          </Dialog.Popup>
        </Dialog.Portal>
      </Dialog.Root>
      <LogoMark />
      <span className="truncate text-s font-heavy tracking-s text-neutral-10">{name}</span>
    </header>
  )
}

type NavItemProps = {
  to: LinkProps['to']
  icon: LucideIcon
  exact?: boolean
  badge?: ReactNode
  children: ReactNode
}

function NavItem({ to, icon: Icon, exact = false, badge, children }: NavItemProps) {
  return (
    <Link
      to={to}
      activeOptions={{ exact, includeSearch: false }}
      className="flex items-center gap-s rounded-s px-s py-xs text-s text-neutral-8 transition-colors hover:bg-neutral-3 hover:text-neutral-10 aria-[current=page]:bg-(--navigation-active-background) aria-[current=page]:font-medium aria-[current=page]:text-(--navigation-active-foreground)"
    >
      <Icon size={16} aria-hidden />
      <span className="min-w-0 flex-1 truncate">{children}</span>
      {badge}
    </Link>
  )
}

function RunningJobsBadge() {
  const running = useQuery({ ...runningJobsQuery, subscribed: useFrameSubscribed() }).data?.total ?? 0
  if (running === 0) return null
  return (
    <span className="flex items-center gap-xxs text-xs font-medium text-color-1">
      <span className="size-[6px] rounded-full bg-color-1" aria-hidden />
      {formatCount(running)}
      <span className="sr-only">running</span>
    </span>
  )
}

function GithubAccount() {
  const installations = useQuery({ ...githubQuery, subscribed: useFrameSubscribed() }).data?.installations
  if (!installations) return null
  // An organization usually holds most of the repositories, so it leads the card.
  const first = installations.find((installation) => installation.account_type === 'Organization') ?? installations[0]
  const others = installations.filter((installation) => installation !== first)
  const repositories = first?.repository_count ?? 0

  return (
    <Link
      to="/settings/github"
      className="flex items-center gap-s rounded-m bg-neutral-1 p-s text-neutral-10 transition-colors hover:bg-neutral-3"
    >
      <GithubIcon size={16} aria-hidden />
      <span className="flex min-w-0 flex-1 flex-col">
        {first ? (
          <>
            <span className="truncate text-s font-medium">
              {first.account_login}
              {others.length > 0 && <span className="text-neutral-7"> +{others.length}</span>}
            </span>
            <span className="truncate text-xs text-neutral-7">
              {first.suspended_at ? 'Suspended' : `${formatCount(repositories)} ${repositories === 1 ? 'repository' : 'repositories'}`}
            </span>
          </>
        ) : (
          <span className="text-s font-medium">Connect GitHub</span>
        )}
      </span>
    </Link>
  )
}
