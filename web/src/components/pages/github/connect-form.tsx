import { Check } from 'lucide-react'
import { useEffect, useId, useMemo, useState } from 'react'
import { $api, errorMessage, useFieldErrors, type Schemas } from '~/api/client'
import { Button } from '~/components/ui/button'
import { Field } from '~/components/ui/field'
import { Form } from '~/components/ui/form'
import { GithubIcon } from '~/components/ui/icons'
import { RadioCard, RadioGroup } from '~/components/ui/radio-group'
import { cx } from '~/lib/cx'
import { OrganizationSearch } from './organization-search'
import { borderedList, borderedRow } from './parts'

type AccountType = Schemas['ManifestRequest']['account_type']

// The permissions of the manifest. Runners of a personal account are
// repository runners, which need Administration.
const permissions: Record<AccountType, ReadonlyArray<[name: string, access: string]>> = {
  organization: [
    ['Self-hosted runners', 'Read and write'],
    ['Actions', 'Read and write'],
    ['Metadata', 'Read'],
  ],
  user: [
    ['Administration', 'Read and write'],
    ['Actions', 'Read and write'],
    ['Metadata', 'Read'],
  ],
}

/**
 * Step 2 of the setup before an app exists. The API returns a GitHub URL and the
 * app manifest; the browser then posts the manifest to GitHub in a plain form,
 * because GitHub's manifest flow takes no other request.
 */
export function ConnectForm() {
  const [accountType, setAccountType] = useState<AccountType>('organization')
  const [organization, setOrganization] = useState<string | null>(null)
  const [unselected, setUnselected] = useState(false)
  const manifest = $api.useMutation('post', '/api/github/manifest', { onSuccess: postToGitHub })
  const installOnId = useId()
  const permissionsId = useId()

  // Back from GitHub, the browser may restore this page with the button still busy.
  const { reset } = manifest
  useEffect(() => {
    const onPageShow = (event: PageTransitionEvent) => event.persisted && reset()
    window.addEventListener('pageshow', onPageShow)
    return () => window.removeEventListener('pageshow', onPageShow)
  }, [reset])

  const serverErrors = useFieldErrors(manifest.error)
  const errors = useMemo(
    () => (unselected ? { ...serverErrors, organization: 'Select the organization in the list.' } : serverErrors),
    [unselected, serverErrors],
  )
  const busy = manifest.isPending || manifest.isSuccess

  return (
    <Form
      errors={errors}
      onFormSubmit={() => {
        if (accountType === 'user') {
          manifest.mutate({ body: { account_type: 'user' } })
        } else if (organization) {
          manifest.mutate({ body: { account_type: 'organization', organization } })
        } else {
          setUnselected(true)
        }
      }}
      className="gap-xl"
    >
      <div className="flex flex-col gap-xl rounded-m bg-neutral-2 p-xl">
        <div className="flex flex-col gap-xs">
          <span id={installOnId} className="text-s font-medium text-neutral-10">
            Install on
          </span>
          <RadioGroup
            aria-labelledby={installOnId}
            direction="row"
            value={accountType}
            onValueChange={(value) => setAccountType(value as AccountType)}
            disabled={busy}
          >
            <RadioCard value="organization" title="Organization" description="Every repository in the organization can use your runners." />
            <RadioCard value="user" title="Personal account" description="Only repositories you own can use your runners." />
          </RadioGroup>
        </div>

        {accountType === 'organization' && (
          <Field
            name="organization"
            label="Organization"
            wide
            description={
              organization
                ? `runplane makes the GitHub App in the organization ${organization}.`
                : 'Type the name from the URL of the organization (acme for github.com/acme), then select the organization in the list.'
            }
            disabled={busy}
          >
            <OrganizationSearch
              value={organization}
              onValueChange={(login) => {
                setOrganization(login)
                setUnselected(false)
              }}
              disabled={busy}
            />
          </Field>
        )}

        <div className="flex flex-col gap-xs">
          <span id={permissionsId} className="text-s font-medium text-neutral-10">
            The app asks for
          </span>
          <ul aria-labelledby={permissionsId} className={borderedList}>
            {permissions[accountType].map(([name, access]) => (
              <li key={name} className={cx('flex items-center gap-s px-m py-xs text-s', borderedRow)}>
                <Check size={14} className="text-neutral-8" aria-hidden />
                <span className="min-w-0 flex-1 text-neutral-10">{name}</span>
                <span className="text-neutral-7">{access}</span>
              </li>
            ))}
          </ul>
        </div>

        <p className="text-s text-neutral-7">runplane polls GitHub for jobs, so this instance needs no public URL.</p>
      </div>

      <div className="flex flex-col gap-s">
        <div className="flex flex-wrap items-center gap-x-m gap-y-s">
          <Button type="submit" variant="primary" loading={busy}>
            {!busy && <GithubIcon aria-hidden />}
            Create GitHub App
          </Button>
          <p className="text-s text-neutral-7">Next, GitHub asks which repositories the app can access.</p>
        </div>
        {manifest.error && Object.keys(errors).length === 0 && (
          <p role="alert" className="text-s text-error">
            {errorMessage(manifest.error)}
          </p>
        )}
      </div>
    </Form>
  )
}

function postToGitHub({ action_url, manifest }: Schemas['ManifestForm']) {
  const form = document.createElement('form')
  form.method = 'post'
  form.action = action_url
  form.hidden = true
  const field = document.createElement('input')
  field.type = 'hidden'
  field.name = 'manifest'
  field.value = manifest
  form.append(field)
  document.body.append(form)
  form.submit()
}
