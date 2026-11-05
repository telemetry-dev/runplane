import { useQuery } from '@tanstack/react-query'
import { Link } from '@tanstack/react-router'
import { ChevronsUpDown, Copy, Settings2, Users } from 'lucide-react'
import { statusQuery, useIsAdmin } from '~/api/session'
import { copyText } from '~/components/app/copy-button'
import { LogoMark } from '~/components/ui/brand'
import { Menu, MenuItem, MenuLinkItem, MenuPopup, MenuSeparator, MenuTrigger } from '~/components/ui/menu'
import { useFrameSubscribed } from './pending'

export function InstanceMenu() {
  const subscribed = useFrameSubscribed()
  const name = useQuery({ ...statusQuery, subscribed }).data?.instance_name || 'runplane'
  // Members is an admin-only page.
  const admin = useIsAdmin({ subscribed })

  return (
    <Menu>
      <MenuTrigger className="-my-xxs flex items-center gap-s rounded-s px-xs py-xxs text-left transition-colors hover:bg-neutral-3 data-popup-open:bg-neutral-3">
        <LogoMark />
        <span className="flex min-w-0 flex-1 flex-col">
          <span className="truncate text-s font-heavy tracking-s text-neutral-10">{name}</span>
          <span className="truncate text-xs text-neutral-7">{location.host}</span>
        </span>
        <ChevronsUpDown size={14} className="text-neutral-7" aria-hidden />
      </MenuTrigger>
      <MenuPopup>
        <MenuLinkItem render={<Link to="/settings/general" />} icon={<Settings2 />}>
          Instance settings
        </MenuLinkItem>
        {admin && (
          <MenuLinkItem render={<Link to="/settings/members" />} icon={<Users />}>
            Members
          </MenuLinkItem>
        )}
        <MenuSeparator />
        <MenuItem icon={<Copy />} onClick={() => void copyText(location.origin)}>
          Copy URL
        </MenuItem>
      </MenuPopup>
    </Menu>
  )
}
