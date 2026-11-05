import { Form as BaseForm } from '@base-ui/react/form'
import { cx } from '~/lib/cx'

type FormProps = Omit<BaseForm.Props, 'className'> & { className?: string }

/**
 * Form of gui/components/runplane/form.md: a stack of Fields with M spacing.
 * Pass server errors as `errors={useFormProblems(mutation.error).fields}`; Base UI shows
 * each one under its field and clears it when the value changes.
 */
export function Form({ className, ...props }: FormProps) {
  return <BaseForm className={cx('flex flex-col gap-m', className)} {...props} />
}
