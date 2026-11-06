import { Field as BaseField } from '@base-ui/react/field'
import { useRef, useState, type KeyboardEvent, type ReactNode } from 'react'
import { Input } from '~/components/ui/input'
import { NumberInput } from '~/components/ui/number-field'
import { Switch } from '~/components/ui/switch'
import type { Schemas } from '~/api/client'
import { cx } from '~/lib/cx'
import { lineNumber, listItems } from './list-text'
import { SettingRow } from './parts'
import { saveError, showSaved, useSaveSettings, useSaveSwitch, type SwitchKey } from './save'

type SettingsPatch = Schemas['SettingsPatch']

// Settings rows whose control saves itself: text and number fields send their
// one key on blur or Enter (Escape puts the saved value back), switches send
// theirs at once.

type RowProps = {
  label: ReactNode
  description: ReactNode
  disabled?: boolean
}

type TextSettingProps = RowProps & {
  value: string
  toPatch: (text: string) => SettingsPatch
  type?: 'text' | 'url' | 'email'
  placeholder?: string
  maxLength?: number
}

export function TextSetting({ label, description, disabled, value, toPatch, type = 'text', placeholder, maxLength }: TextSettingProps) {
  const setting = useSetting(value, toPatch)
  return (
    <SettingRow label={label} description={description} disabled={disabled} error={setting.error}>
      {/* Input is full width, so a wrapper sets the width. */}
      <div className="w-[280px]">
        <Input
          type={type}
          value={setting.value}
          onValueChange={setting.change}
          onBlur={setting.commit}
          onKeyDown={setting.keyDown}
          placeholder={placeholder}
          maxLength={maxLength}
          autoComplete="off"
          spellCheck={false}
        />
      </div>
    </SettingRow>
  )
}

type ListSettingProps = RowProps & {
  value: string[]
  toPatch: (items: string[]) => SettingsPatch
  /** One item on each line in a textarea, instead of a comma-separated input. */
  lines?: boolean
  /** A message for a list that the API would refuse, before sending it. */
  check?: (items: string[]) => string | undefined
  placeholder?: string
}

/** A list of short values, as the DNS servers or the egress allowlist. */
export function ListSetting({ label, description, disabled, value, toPatch, lines = false, check, placeholder }: ListSettingProps) {
  const separator = lines ? '\n' : ', '
  const setting = useSetting(value.join(separator), (text) => toPatch(listItems(text, lines)), {
    check: check && ((text) => check(listItems(text, lines))),
    // The API reports list errors by index; a textarea shows the line it means.
    line: lines ? (index, message, text) => `Line ${lineNumber(text, index)}: ${message}` : undefined,
  })
  const control = lines ? (
    <BaseField.Control
      render={<textarea rows={4} />}
      value={setting.value}
      onValueChange={setting.change}
      onBlur={setting.commit}
      onKeyDown={setting.keyDown}
      placeholder={placeholder}
      spellCheck={false}
      className={cx(
        'h-[96px] w-[280px] resize-none rounded-s bg-(--input-background,var(--neutral-1)) px-s py-xs text-s text-neutral-10',
        'transition-[background-color,color] placeholder:text-neutral-7 data-disabled:bg-neutral-3 data-disabled:text-neutral-6',
      )}
    />
  ) : (
    <div className="w-[280px]">
      <Input
        value={setting.value}
        onValueChange={setting.change}
        onBlur={setting.commit}
        onKeyDown={setting.keyDown}
        placeholder={placeholder}
        autoComplete="off"
        spellCheck={false}
      />
    </div>
  )
  return (
    <SettingRow label={label} description={description} disabled={disabled} error={setting.error} alignTop={lines}>
      {control}
    </SettingRow>
  )
}

type NumberSettingProps = RowProps & NumberOptions

/** A number with its unit, as the retention days or the CPU overcommit. */
export function NumberSetting({ label, description, disabled, ...options }: NumberSettingProps) {
  const setting = useNumberSetting({ ...options, disabled })
  return (
    <SettingRow label={label} description={description} disabled={disabled} error={setting.error}>
      {setting.field}
    </SettingRow>
  )
}

type NumberOptions = {
  value: number
  toPatch: (value: number) => SettingsPatch
  unit?: ReactNode
  /** The range of the value, as the API limits of the setting. */
  limit: { minimum: number; maximum: number }
  step?: number
  /** How the field shows the value; whole numbers by default. Its maximum decimals are also the ones it keeps. */
  format?: Intl.NumberFormatOptions
  /** Shown for 0, which then means "none", as "No limit". An empty field saves 0. */
  zeroPlaceholder?: string
  disabled?: boolean
  /** For a field in a row with more than one (Host reserve). */
  'aria-label'?: string
  className?: string
}

/**
 * The field of a number setting and its save error. Base UI clamps and rounds
 * on blur; Enter does the same here before it saves.
 */
export function useNumberSetting({
  value,
  toPatch,
  limit: { minimum: min, maximum: max },
  step = 1,
  format = { maximumFractionDigits: 0 },
  zeroPlaceholder,
  unit,
  className = 'w-[120px]',
  ...props
}: NumberOptions) {
  const setting = useSetting<number | null>(zeroPlaceholder && value === 0 ? null : value, (next) =>
    next === null ? (zeroPlaceholder ? toPatch(0) : undefined) : toPatch(next),
  )
  // Base UI keeps the typed text while the input has focus, also when the
  // value changes. Escape thus mounts a new field, which shows the saved value.
  const [resets, setResets] = useState(0)

  function keyDown(event: KeyboardEvent<HTMLInputElement>) {
    const next = setting.latest()
    if (event.key === 'Enter' && next !== null) {
      setting.change(round(Math.min(max, Math.max(min, next)), format.maximumFractionDigits ?? 0))
    }
    if (event.key === 'Escape' && setting.changed) setResets((count) => count + 1)
    setting.keyDown(event)
  }

  const field = (
    <div className="contents" onBlur={setting.commit}>
      <NumberInput
        key={resets}
        // The new field takes the focus of the one it replaces.
        autoFocus={resets > 0}
        value={setting.value}
        onValueChange={setting.change}
        onKeyDown={keyDown}
        min={min}
        max={max}
        step={step}
        format={{ useGrouping: false, ...format }}
        placeholder={zeroPlaceholder}
        suffix={unit}
        className={className}
        {...props}
      />
    </div>
  )
  return { field, error: setting.error }
}

type SwitchSettingProps = RowProps & {
  name: SwitchKey
  checked: boolean
}

/** A switch row. It moves at once, and moves back with the reason if the API refuses the change. */
export function SwitchSetting({ name, label, description, disabled, checked }: SwitchSettingProps) {
  const save = useSaveSwitch(name)
  return (
    <Switch
      label={label}
      description={description}
      error={saveError(save.error)}
      checked={checked}
      disabled={disabled}
      onCheckedChange={(next) => save.mutate(next)}
      className="py-m"
    />
  )
}

type SettingOptions<T> = {
  /** A message for a value that the API would refuse, before sending it. */
  check?: (value: T) => string | undefined
  /** The message for the API error of list item `index`. */
  line?: (index: number, message: string, value: T) => string
}

/**
 * The value of a control that saves on commit. It shows the saved value until
 * the user changes it, and keeps a refused value so the user can fix it.
 * `toPatch` returns undefined for a value that cannot be saved (an empty
 * number field); the saved value then comes back.
 */
function useSetting<T>(saved: T, toPatch: (value: T) => SettingsPatch | undefined, { check, line }: SettingOptions<T> = {}) {
  const save = useSaveSettings()
  const [draft, setDraft] = useState<{ value: T }>()
  const [invalid, setInvalid] = useState<string>()
  // The newest value, also when it changes in the same event as the commit (Base UI clamps on blur).
  const latest = useRef<T>(saved)
  const sent = useRef<{ value: T }>(undefined)

  function change(value: T) {
    latest.current = value
    setDraft({ value })
    setInvalid(undefined)
  }

  function revert() {
    latest.current = saved
    setDraft(undefined)
    setInvalid(undefined)
    save.reset()
  }

  function commit() {
    if (!draft) return
    const value = latest.current
    if (value === saved) return revert()
    if (save.isPending && sent.current?.value === value) return
    const message = check?.(value)
    if (message) return setInvalid(message)
    const body = toPatch(value)
    if (!body) return revert()
    sent.current = { value }
    save.mutate(
      { body },
      {
        onSuccess: () => {
          if (latest.current === value) setDraft(undefined)
          showSaved()
        },
      },
    )
  }

  function keyDown(event: KeyboardEvent<HTMLElement>) {
    const textarea = event.currentTarget instanceof HTMLTextAreaElement
    if (event.key === 'Enter' && (!textarea || event.metaKey || event.ctrlKey)) {
      event.preventDefault()
      commit()
    }
    if (event.key === 'Escape' && draft) {
      event.preventDefault()
      revert()
    }
  }

  const value = draft ? draft.value : saved
  return {
    value,
    /** The user changed the value and did not save it yet. */
    changed: draft !== undefined,
    latest: () => (draft ? latest.current : saved),
    change,
    commit,
    keyDown,
    error: invalid ?? saveError(save.error, line && ((index, message) => line(index, message, value))),
  }
}

function round(value: number, decimals: number): number {
  const factor = 10 ** decimals
  return Math.round(value * factor) / factor
}
