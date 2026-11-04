import type { ReactNode } from 'react'

/**
 * A runner or label name in the code font of the designs, for the title line of
 * a CellTitle: XS code text on the S line height. It is a block, so the line box
 * holds only the code font and rows keep their height.
 */
export function Code({ children }: { children: ReactNode }) {
  return <span className="block truncate font-code text-xs leading-s font-regular tracking-m">{children}</span>
}
