import { Check } from 'lucide-react'
import { useEffect, useId, useState } from 'react'
import { $api, errorMessage, useFieldErrors, type Schemas } from '~/api/client'
import { SectionButton } from '~/components/app/section-header'
import { Button } from '~/components/ui/button'
import { Field, Input } from '~/components/ui/field'
import { Form } from '~/components/ui/form'
import { GithubIcon } from '~/components/ui/icons'
import { RadioCard, RadioGroup } from '~/components/ui/radio-group'
import { cx } from '~/lib/cx'
import { AccountPicker, useAccountSignIn } from './account-picker'
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
 *
 * With `canSignIn`, the user signs in to GitHub and picks the account from a
 * list. Otherwise, or after "Type the name", the user types the organization:
 * GitHub makes the app on the personal account for a name that is wrong.
 */
export function ConnectForm({ canSignIn }: { canSignIn: boolean }) {
  const [typed, setTyped] = useState(!canSignIn)
  const [accountType, setAccountType] = useState<AccountType>('organization')
  const signIn = useAccountSignIn()
  const [picked, setPicked] = useState<string>()
  // The list puts the organizations that the user owns first, where most apps go.
  const account = signIn.accounts?.find((a) => a.login === picked) ?? signIn.accounts?.[0]
  const shownType = typed ? accountType : (account?.type ?? 'organization')
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

  const errors = useFieldErrors(manifest.error)
  const busy = manifest.isPending || manifest.isSuccess

  return (
    <Form
      errors={errors}
      onFormSubmit={(values) => {
        const type = typed ? accountType : account?.type
        if (!type) return
        const organization = typed ? String(values.organization).trim() : account?.login
        manifest.mutate({ body: type === 'organization' ? { account_type: type, organization } : { account_type: type } })
      }}
      className="gap-xl"
    >
      <div className="flex flex-col gap-xl rounded-m bg-neutral-2 p-xl">
        <div className="flex flex-col gap-xs">
          <div className="flex items-center justify-between gap-m">
            <span id={installOnId} className="text-s font-medium text-neutral-10">
              Install on
            </span>
            {canSignIn && (
              <SectionButton onClick={() => setTyped(!typed)} disabled={busy}>
                {typed ? 'Sign in to GitHub' : 'Type the name'}
              </SectionButton>
            )}
          </div>
          {typed ? (
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
          ) : (
            <AccountPicker signIn={signIn} labelId={installOnId} value={account} onValueChange={setPicked} disabled={busy} />
          )}
        </div>

        {typed && accountType === 'organization' && (
          <Field name="organization" label="Organization" validate={validateOrganization} disabled={busy}>
            <Input required autoComplete="off" spellCheck={false} autoCapitalize="none" maxLength={39} />
          </Field>
        )}

        <div className="flex flex-col gap-xs">
          <span id={permissionsId} className="text-s font-medium text-neutral-10">
            The app asks for
          </span>
          <ul aria-labelledby={permissionsId} className={borderedList}>
            {permissions[shownType].map(([name, access]) => (
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
          <Button type="submit" variant="primary" loading={busy} disabled={!typed && !account}>
            {!busy && <GithubIcon aria-hidden />}
            Create GitHub App
          </Button>
          <p className="text-s text-neutral-7">Next, GitHub asks which repositories the app can access.</p>
        </div>
        {/* Without the organization field, its error shows here. */}
        {manifest.error && (!typed || Object.keys(errors).length === 0) && (
          <p role="alert" className="text-s text-error">
            {errorMessage(manifest.error)}
          </p>
        )}
      </div>
    </Form>
  )
}

function validateOrganization(value: unknown): string | null {
  const login = String(value ?? '').trim()
  if (!login) return 'Enter the organization.'
  if (!/^[A-Za-z0-9][A-Za-z0-9-]*$/.test(login)) return 'Use the name from the organization URL: letters, digits, and hyphens.'
  return null
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
