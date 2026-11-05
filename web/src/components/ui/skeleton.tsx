import type { CSSProperties } from 'react'
import { cx } from '~/lib/cx'

/** A loading placeholder; give it the size of the text or control it stands for. */
export function Skeleton({ className, style }: { className: string; style?: CSSProperties }) {
  return <span aria-hidden className={cx('block animate-pulse rounded-xs bg-neutral-3', className)} style={style} />
}
