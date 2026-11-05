import { Avatar as BaseAvatar } from '@base-ui/react/avatar'
import { cx } from '~/lib/cx'

type AvatarProps = {
  /** A person or account name; the fallback shows its initials. */
  name: string
  src?: string
  /** `m` is the 24px avatar of the sidebar; `l` is the 28px account avatar of Settings · GitHub. */
  size?: 'm' | 'l'
  className?: string
}

export function Avatar({ name, src, size = 'm', className }: AvatarProps) {
  return (
    <BaseAvatar.Root
      className={cx(
        'grid shrink-0 place-items-center overflow-hidden rounded-full bg-neutral-3 text-xs text-neutral-8 select-none',
        size === 'm' ? 'size-[24px] font-medium' : 'size-[28px] font-heavy',
        className,
      )}
    >
      {src && <BaseAvatar.Image src={src} alt="" className="size-full object-cover" />}
      <BaseAvatar.Fallback>{initials(name, size === 'm' ? 2 : 1)}</BaseAvatar.Fallback>
    </BaseAvatar.Root>
  )
}

/** "Ada Okafor" gives "AO", "kestrel-hq" gives "K", "ada@example.com" gives "A"; `max` 1 keeps the first letter. */
function initials(name: string, max: number): string {
  const words = name.split('@')[0]!.trim().split(/\s+/).filter(Boolean)
  const first = words[0]?.[0] ?? ''
  const last = max > 1 && words.length > 1 ? (words.at(-1)?.[0] ?? '') : ''
  return (first + last).toUpperCase()
}
