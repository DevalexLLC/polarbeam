import { inheritRouteNetwork } from '../routeState'
import PageError from './PageError'

export default function SettingsPageError({
  title,
  subject,
  error,
  onRetry,
}: {
  title: string
  subject: string
  error: unknown
  onRetry: () => void
}) {
  return (
    <PageError
      title={title}
      subject={subject}
      error={error}
      backHref={inheritRouteNetwork('#/settings')}
      backLabel="Back to Settings"
      onRetry={onRetry}
      headingLevel={2}
    />
  )
}
