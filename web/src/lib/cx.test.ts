import { expect, test } from 'vitest'
import { cx } from './cx'

test.each<[Parameters<typeof cx>, string]>([
  // A caller's class replaces the component's class for the same property.
  [['flex flex-wrap gap-x-xxl gap-y-m', 'gap-x-xl'], 'flex flex-wrap gap-y-m gap-x-xl'],
  [['flex gap-xs text-s text-neutral-8', 'text-neutral-7'], 'flex gap-xs text-s text-neutral-7'],
  [['rounded-s shadow-s', 'rounded-m shadow-[0_0_0_1px_var(--neutral-4)]'], 'rounded-m shadow-[0_0_0_1px_var(--neutral-4)]'],
  [['text-s text-neutral-8', 'text-xs'], 'text-neutral-8 text-xs'],
  [['type-body', 'type-label'], 'type-label'],
  // Theme names of different properties all stay.
  [['text-s font-heavy font-code tracking-m leading-s text-neutral-10 bg-color-1-transparent'], 'text-s font-heavy font-code tracking-m leading-s text-neutral-10 bg-color-1-transparent'],
  // A text size sets the line height only as a fallback, so an earlier leading class stays.
  [['leading-s', 'text-xs'], 'leading-s text-xs'],
  [['min-w-0', 'min-w-zero', false, undefined], 'min-w-zero'],
])('cx(%j) is %j', (classes, merged) => {
  expect(cx(...classes)).toBe(merged)
})
