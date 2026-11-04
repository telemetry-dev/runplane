import { extendTailwindMerge, type ClassNameValue } from 'tailwind-merge'

/**
 * Joins class names and drops empty ones. Of two classes that set the same
 * property, the later one stays, so a caller's className wins over the
 * component's own: `cx('gap-x-xxl text-neutral-8', 'gap-x-xl text-neutral-7')`.
 */
export function cx(...classes: ClassNameValue[]): string {
  return merge(...classes)
}

// tailwind-merge must know the theme names of styles/theme.css. Without them it
// reads `text-s` as a color and `font-heavy` as a font family, and drops classes
// that do not conflict.
const sizes = ['xxs', 'xs', 's', 'm', 'l', 'xl', 'xxl']
const colors = [
  ...['neutral-1', 'neutral-2', 'neutral-3', 'neutral-4', 'neutral-5', 'neutral-6', 'neutral-7', 'neutral-8', 'neutral-9', 'neutral-10'],
  ...['color-1', 'color-2', 'color-3', 'color-4', 'success', 'warning', 'error'],
].flatMap((name) => [name, `${name}-transparent`])

const merge = extendTailwindMerge<'type-role'>({
  extend: {
    theme: { spacing: ['zero', ...sizes] },
    classGroups: { 'type-role': [{ type: ['display', 'page-title', 'section', 'body', 'label', 'meta', 'stat', 'code'] }] },
  },
  override: {
    theme: {
      color: ['inherit', 'current', 'transparent', ...colors],
      text: sizes,
      leading: sizes,
      tracking: ['xs', 's', 'm', 'l', 'xl'],
      font: ['ui', 'brand', 'editorial', 'data', 'code'],
      'font-weight': ['regular', 'medium', 'heavy'],
      radius: ['zero', 'xs', 's', 'm', 'l', 'xl', 'full'],
      shadow: ['s', 'm', 'l'],
      ease: ['small', 'large'],
    },
    // A text size sets the line height only as a fallback, so a leading class wins in any order.
    conflictingClassGroups: { 'font-size': [] },
  },
})
