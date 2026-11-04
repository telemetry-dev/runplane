import { Spinner } from '~/components/ui/spinner'

export type ServerStatus = 'online' | 'installing' | 'unreachable' | 'draining' | 'disabled'

type StatusDotProps = {
  status: ServerStatus
  /** Pixels of the slot. The dot is 8px; tables use a 16px slot so it lines up with icons. */
  size?: number
  className?: string
}

/**
 * Server state of the designs: a filled dot when online, a hollow ring when
 * unreachable or disabled, a half-filled dot while draining, and the spinner
 * while installing. Pair it with the state in words; the dot alone is not enough.
 */
export function StatusDot({ status, size = 16, className }: StatusDotProps) {
  if (status === 'installing') return <Spinner size={size} label={labels.installing} className={className} />
  return (
    <svg width={size} height={size} viewBox="0 0 16 16" role="img" aria-label={labels[status]} className={className} style={{ flexShrink: 0 }}>
      {status === 'online' && <circle cx="8" cy="8" r="4" fill="var(--neutral-10)" />}
      {status === 'draining' && (
        <>
          <circle cx="8" cy="8" r="3.25" fill="none" stroke="var(--neutral-10)" strokeWidth="1.5" />
          <path d="M8 4a4 4 0 0 0 0 8z" fill="var(--neutral-10)" />
        </>
      )}
      {(status === 'unreachable' || status === 'disabled') && (
        <circle cx="8" cy="8" r="3.5" fill="none" stroke="var(--neutral-6)" strokeWidth="1.5" />
      )}
    </svg>
  )
}

const labels: Record<ServerStatus, string> = {
  online: 'Online',
  installing: 'Installing',
  unreachable: 'Unreachable',
  draining: 'Draining',
  disabled: 'Disabled',
}
