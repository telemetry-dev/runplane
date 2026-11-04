import { Menu as BaseMenu } from '@base-ui/react/menu'
import { Check, ChevronRight } from 'lucide-react'
import type { ReactNode } from 'react'
import { cx } from '~/lib/cx'
import { option, popupMotion, popupSurface } from './popup'

export const Menu = BaseMenu.Root
export const MenuTrigger = BaseMenu.Trigger
export const MenuGroup = BaseMenu.Group
export const MenuRadioGroup = BaseMenu.RadioGroup
/** A nested menu: `<MenuSubmenu><MenuSubmenuTrigger>…</MenuSubmenuTrigger><MenuPopup side="right">…</MenuPopup></MenuSubmenu>`. */
export const MenuSubmenu = BaseMenu.SubmenuRoot

type MenuPopupProps = Pick<BaseMenu.Positioner.Props, 'side' | 'align' | 'sideOffset'> & {
  className?: string
  children: ReactNode
}

/** Popup of gui/components/runplane/menu.md. */
export function MenuPopup({ side = 'bottom', align = 'start', sideOffset = 4, className, children }: MenuPopupProps) {
  return (
    <BaseMenu.Portal>
      <BaseMenu.Positioner side={side} align={align} sideOffset={sideOffset} className="z-20 outline-none">
        <BaseMenu.Popup
          className={cx(
            'flex flex-col gap-xxs overflow-y-auto overscroll-contain rounded-m p-xxs',
            'max-h-[var(--available-height,360px)] min-w-[min(220px,var(--available-width))]',
            'max-w-[min(var(--available-width),calc(100vw-var(--space-xl)),420px)]',
            popupSurface,
            popupMotion,
            className,
          )}
        >
          {children}
        </BaseMenu.Popup>
      </BaseMenu.Positioner>
    </BaseMenu.Portal>
  )
}

// Options use a grid with a 1em icon column, so labels line up with and without icons.
const menuOption = cx(option, 'grid grid-cols-[1em_1fr_auto] items-center gap-xs [&_svg]:size-[1em]')

type MenuItemProps = Omit<BaseMenu.Item.Props, 'className' | 'children'> & {
  icon?: ReactNode
  /** Trailing content in detail type, for example a shortcut or a count. */
  detail?: ReactNode
  /** A destructive action, such as Remove. */
  danger?: boolean
  children: ReactNode
}

export function MenuItem({ icon, detail, danger = false, children, ...props }: MenuItemProps) {
  return (
    <BaseMenu.Item className={cx(menuOption, danger && 'not-data-disabled:text-error')} {...props}>
      <OptionContent icon={icon} detail={detail} danger={danger}>
        {children}
      </OptionContent>
    </BaseMenu.Item>
  )
}

type MenuLinkItemProps = Omit<BaseMenu.LinkItem.Props, 'className' | 'children'> & {
  icon?: ReactNode
  detail?: ReactNode
  children: ReactNode
}

/** A menu link. For a router link, pass `render={<Link to="..." />}`. */
export function MenuLinkItem({ icon, detail, children, ...props }: MenuLinkItemProps) {
  return (
    <BaseMenu.LinkItem className={menuOption} closeOnClick {...props}>
      <OptionContent icon={icon} detail={detail}>
        {children}
      </OptionContent>
    </BaseMenu.LinkItem>
  )
}

type MenuRadioItemProps = Omit<BaseMenu.RadioItem.Props, 'className' | 'children'> & { children: ReactNode }

export function MenuRadioItem({ children, ...props }: MenuRadioItemProps) {
  return (
    <BaseMenu.RadioItem className={menuOption} {...props}>
      <BaseMenu.RadioItemIndicator keepMounted className="flex data-unchecked:invisible">
        <Check aria-hidden />
      </BaseMenu.RadioItemIndicator>
      <span className="truncate">{children}</span>
    </BaseMenu.RadioItem>
  )
}

type MenuCheckboxItemProps = Omit<BaseMenu.CheckboxItem.Props, 'className' | 'children'> & { children: ReactNode }

export function MenuCheckboxItem({ children, ...props }: MenuCheckboxItemProps) {
  return (
    <BaseMenu.CheckboxItem className={menuOption} {...props}>
      <BaseMenu.CheckboxItemIndicator keepMounted className="flex data-unchecked:invisible">
        <Check aria-hidden />
      </BaseMenu.CheckboxItemIndicator>
      <span className="truncate">{children}</span>
    </BaseMenu.CheckboxItem>
  )
}

type MenuSubmenuTriggerProps = Omit<BaseMenu.SubmenuTrigger.Props, 'className' | 'children'> & {
  icon?: ReactNode
  children: ReactNode
}

/** Opens a nested menu; it keeps the highlight while its submenu is open. */
export function MenuSubmenuTrigger({ icon, children, ...props }: MenuSubmenuTriggerProps) {
  return (
    <BaseMenu.SubmenuTrigger className={cx(menuOption, 'data-popup-open:bg-neutral-3')} {...props}>
      <OptionContent icon={icon} detail={<ChevronRight aria-hidden />}>
        {children}
      </OptionContent>
    </BaseMenu.SubmenuTrigger>
  )
}

export function MenuGroupLabel({ children }: { children: ReactNode }) {
  return <BaseMenu.GroupLabel className="px-xs pt-xs pb-xxs text-xs font-medium text-neutral-6">{children}</BaseMenu.GroupLabel>
}

/** Read-only text at the top of a menu, for example the signed-in email. */
export function MenuNote({ children }: { children: ReactNode }) {
  return <div className="truncate px-xs pt-xs pb-xxs text-xs text-neutral-7">{children}</div>
}

export function MenuSeparator() {
  return <BaseMenu.Separator className="my-xxs h-px shrink-0 shadow-[inset_0_1px_0_var(--neutral-4)]" />
}

type OptionContentProps = { icon?: ReactNode; detail?: ReactNode; danger?: boolean; children: ReactNode }

// Icons are neutral-8 next to the neutral-10 label, as in the designs; a destructive item keeps its color.
function OptionContent({ icon, detail, danger = false, children }: OptionContentProps) {
  return (
    <>
      <span className={cx('flex', !danger && 'text-neutral-8', 'in-data-disabled:text-neutral-6')} aria-hidden>
        {icon}
      </span>
      <span className="truncate">{children}</span>
      {detail && <span className="ml-s flex text-xs text-neutral-6">{detail}</span>}
    </>
  )
}
