import { Link, type LinkProps } from '@tanstack/react-router'
import type { ReactNode } from 'react'
import { errorMessage } from '~/api/client'
import { Button, buttonClassName } from '~/components/ui/button'

type LoadErrorProps = {
  /** What did not load, as "the job" or "images". */
  what: string
  error: unknown
  onRetry: () => void
  retrying?: boolean
  /** Gives the message a heading of this level, in place of a page (1) or of a page body (2). */
  heading?: 1 | 2
}

/** In place of content whose data did not load: what did not load, why, and a retry. */
export function LoadError({ what, error, onRetry, retrying = false, heading }: LoadErrorProps) {
  const retry = (
    <Button loading={retrying} onClick={onRetry}>
      Try again
    </Button>
  )
  if (heading) {
    const Heading = heading === 1 ? 'h1' : 'h2'
    return (
      <div role="alert" className="flex flex-col items-start gap-m">
        <Heading className={heading === 1 ? 'type-page-title text-neutral-10' : 'type-section text-neutral-10'}>Could not load {what}</Heading>
        <p className="text-s text-neutral-8">{errorMessage(error)}</p>
        {retry}
      </div>
    )
  }
  return (
    <div role="alert" className="flex flex-wrap items-center justify-between gap-m py-m text-s text-neutral-8">
      <p className="min-w-0">
        Could not load {what}. {errorMessage(error)}
      </p>
      {retry}
    </div>
  )
}

/** LoadError in place of the rows of a DataTable. */
export function LoadErrorRow(props: Omit<LoadErrorProps, 'heading'>) {
  return (
    <div role="row" className="px-m">
      <div role="cell">
        <LoadError {...props} />
      </div>
    </div>
  )
}

type NotFoundProps = {
  title: string
  /** Why the object is not there. */
  children: ReactNode
  /** The list of these objects, and the label of the link to it. */
  to: LinkProps['to']
  link: string
}

/** In place of a detail page whose object does not exist (the API answers 404). */
export function NotFound({ title, children, to, link }: NotFoundProps) {
  return (
    <div role="alert" className="flex flex-col items-start gap-m">
      <h1 className="type-page-title text-neutral-10">{title}</h1>
      <p className="text-s text-neutral-8">{children}</p>
      <Link to={to} className={buttonClassName('secondary')}>
        {link}
      </Link>
    </div>
  )
}
