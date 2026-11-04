import { createLink } from '@tanstack/react-router'
import { ChevronRight } from 'lucide-react'
import { Children, Fragment, type AnchorHTMLAttributes, type ReactNode, type Ref } from 'react'

// Trail above a detail page title, as "Jobs › kestrel-hq/web › e2e.yml #2290":
//
//   <Breadcrumbs>
//     <BreadcrumbLink to="/jobs">Jobs</BreadcrumbLink>
//     <BreadcrumbLink to="/jobs" search={{ repository: 'kestrel-hq/web' }}>kestrel-hq/web</BreadcrumbLink>
//     <BreadcrumbPage>e2e.yml #2290</BreadcrumbPage>
//   </Breadcrumbs>

export function Breadcrumbs({ children }: { children: ReactNode }) {
  const items = Children.toArray(children)
  return (
    <nav aria-label="Breadcrumbs">
      <ol className="flex min-w-0 flex-wrap items-center gap-xs text-s text-neutral-7">
        {items.map((item, index) => (
          <Fragment key={index}>
            {index > 0 && (
              <li aria-hidden className="flex">
                <ChevronRight size={12} className="text-neutral-6" />
              </li>
            )}
            <li className="flex min-w-0">{item}</li>
          </Fragment>
        ))}
      </ol>
    </nav>
  )
}

function BreadcrumbAnchor({ ref, ...props }: AnchorHTMLAttributes<HTMLAnchorElement> & { ref?: Ref<HTMLAnchorElement> }) {
  return <a ref={ref} className="truncate rounded-xs transition-colors hover:text-neutral-10" {...props} />
}

/** A step of the trail that links to a route. */
export const BreadcrumbLink = createLink(BreadcrumbAnchor)

/** The current page, last in the trail. */
export function BreadcrumbPage({ children }: { children: ReactNode }) {
  return (
    <span aria-current="page" className="truncate">
      {children}
    </span>
  )
}
