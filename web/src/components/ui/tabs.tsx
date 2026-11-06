import { Tabs as BaseTabs } from '@base-ui/react/tabs'
import type { ReactNode } from 'react'
import { cx } from '~/lib/cx'

// Tabs of gui/components/runplane/tabs.md: the pill strip of the Settings and
// "Add server" designs.
//
//   <Tabs defaultValue="script">
//     <TabsList aria-label="Connection">
//       <Tab value="script">Install script</Tab>
//       <Tab value="ssh">Connect over SSH</Tab>
//     </TabsList>
//     <TabsPanel value="script">…</TabsPanel>
//     <TabsPanel value="ssh">…</TabsPanel>
//   </Tabs>

export const Tabs = BaseTabs.Root

export const tabList = 'relative isolate inline-flex max-w-full self-start gap-xxs overflow-x-auto rounded-m bg-neutral-3 p-xxs'
export const tabItem = cx(
  'rounded-s px-s py-xxs text-s font-medium whitespace-nowrap text-neutral-7 select-none',
  'transition-[background-color,color,box-shadow] hover:text-neutral-10 data-disabled:text-neutral-6',
)

type TabsListProps = Omit<BaseTabs.List.Props, 'className'> & { className?: string }

export function TabsList({ className, children, ...props }: TabsListProps) {
  return (
    <BaseTabs.List className={cx(tabList, className)} {...props}>
      {children}
      <BaseTabs.Indicator
        className={cx(
          'absolute top-(--active-tab-top) left-(--active-tab-left) -z-1 h-(--active-tab-height) w-(--active-tab-width)',
          'rounded-s bg-neutral-1 shadow-s transition-[left,width]',
        )}
      />
    </BaseTabs.List>
  )
}

type TabProps = Omit<BaseTabs.Tab.Props, 'className'> & { className?: string }

export function Tab({ className, ...props }: TabProps) {
  return <BaseTabs.Tab className={cx(tabItem, 'data-active:text-neutral-10', className)} {...props} />
}

type TabsPanelProps = Omit<BaseTabs.Panel.Props, 'className'> & { className?: string; children: ReactNode }

export function TabsPanel({ className, ...props }: TabsPanelProps) {
  return <BaseTabs.Panel className={cx('mt-s flex flex-col gap-s outline-none', className)} {...props} />
}
