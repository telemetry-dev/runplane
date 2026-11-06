import { createFileRoute } from '@tanstack/react-router'
import { ServerStep } from '~/components/pages/setup/server-step'
import { pageHead } from '~/lib/head'

export const Route = createFileRoute('/_setup/setup/')({
  head: () => pageHead('Create your account'),
  component: ServerStep,
})
