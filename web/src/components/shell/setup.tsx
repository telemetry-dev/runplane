import { Check } from 'lucide-react'
import { Fragment, type ReactNode } from 'react'
import { LogoMark } from '~/components/ui/brand'
import { cx } from '~/lib/cx'

export function SetupFrame({ children }: { children: ReactNode }) {
  return (
    <div className="flex min-h-dvh flex-col bg-neutral-1">
      <header className="flex items-center justify-between px-xl py-l">
        <div className="flex items-center gap-s">
          <LogoMark />
          <span className="text-s font-heavy tracking-s text-neutral-10">runplane</span>
        </div>
        <span className="text-s text-neutral-7">{location.host}</span>
      </header>
      <div className="flex flex-col items-center px-xl pb-xxl">{children}</div>
    </div>
  )
}

export function SetupColumn({ children }: { children: ReactNode }) {
  return <main className="mt-xxl flex w-full max-w-[560px] flex-col gap-xl">{children}</main>
}

const setupSteps = [
  { label: 'Server', path: '/setup' },
  { label: 'GitHub', path: '/setup/github' },
  { label: 'First job', path: '/setup/first-job' },
] as const

/** The setup step of a path, counted from 1. */
export function setupStep(pathname: string): number {
  return setupSteps.findLastIndex((step) => pathname.startsWith(step.path)) + 1
}

/** The three setup steps; steps before `current` show as done. `current` counts from 1. */
export function SetupStepper({ current }: { current: number }) {
  return (
    // The connectors shrink first, so the steps fit on one line on a phone.
    <ol aria-label="Setup steps" className="mt-xxl flex max-w-full items-center gap-s">
      {setupSteps.map(({ label }, index) => {
        const step = index + 1
        const state = step < current ? 'done' : step === current ? 'active' : 'next'
        return (
          <Fragment key={label}>
            {index > 0 && <li aria-hidden className="h-px w-[48px] min-w-xs bg-neutral-4" />}
            <li aria-current={state === 'active' ? 'step' : undefined} className="flex shrink-0 items-center gap-xs whitespace-nowrap">
              <span
                className={cx(
                  'grid size-[20px] shrink-0 place-items-center rounded-full text-xs font-heavy tracking-m',
                  state === 'next' ? 'bg-neutral-3 text-neutral-7' : 'bg-neutral-10 text-neutral-1',
                )}
              >
                {state === 'done' ? <Check size={12} className="[--icon-stroke-width:2.5]" aria-label="Done" /> : step}
              </span>
              <span
                className={cx(
                  'text-s font-medium',
                  state === 'done' && 'text-neutral-8',
                  state === 'active' && 'text-neutral-10',
                  state === 'next' && 'text-neutral-7',
                )}
              >
                {label}
              </span>
            </li>
          </Fragment>
        )
      })}
    </ol>
  )
}
