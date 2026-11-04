import { Webhook } from 'lucide-react'
import { DiscordIcon, SlackIcon } from '~/components/ui/icons'
import type { ChannelKind } from './channel-info'

/** The 28px tile with the icon of a channel kind. */
export function ChannelTile({ kind }: { kind: ChannelKind }) {
  const Icon = kind === 'slack' ? SlackIcon : kind === 'discord' ? DiscordIcon : Webhook
  return (
    <span className="grid size-[28px] shrink-0 place-items-center rounded-s bg-neutral-3 text-neutral-10" aria-hidden>
      <Icon size={16} />
    </span>
  )
}
