import { cx } from '~/lib/cx'

// Shared styles of the anchored popups (menu, select, combobox, popover).

/**
 * The theme edge is 0px and its shadow is transparent, so popups add the 1px
 * neutral-4 line that the designs use on tables; without it a neutral-1 popup
 * vanishes on a neutral-1 page.
 */
export const popupSurface = cx(
  'bg-neutral-1 text-s text-neutral-10 outline-none dark:bg-neutral-2',
  'shadow-[0_0_0_1px_var(--neutral-4),var(--shadow-m)]',
)

export const popupMotion = cx(
  'origin-(--transform-origin) transition-[opacity,scale]',
  'data-starting-style:scale-(--motion-popup-scale) data-starting-style:opacity-0',
  'data-ending-style:scale-(--motion-popup-scale) data-ending-style:opacity-0',
)

/** Option rows of menus, selects, and comboboxes. */
export const option = cx(
  'cursor-default rounded-xs px-xs py-xxs outline-none select-none',
  'data-highlighted:bg-neutral-3 data-disabled:text-neutral-6',
)
