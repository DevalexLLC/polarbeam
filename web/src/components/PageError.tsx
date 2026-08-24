import { useEffect, useId, useRef, useState } from 'react'
import { pageFailure } from '../pageState'

export default function PageError({
  title,
  subject,
  error,
  message,
  backHref,
  backLabel,
  onRetry,
  headingLevel = 1,
}: {
  title: string
  subject: string
  error?: unknown
  message?: string
  backHref?: string
  backLabel?: string
  onRetry?: () => void
  headingLevel?: 1 | 2
}) {
  const headingRef = useRef<HTMLHeadingElement>(null)
  const headingID = useId()
  const [retryingError, setRetryingError] = useState<unknown>(null)
  const failure = message === undefined ? pageFailure(error, subject) : { message, retryable: false }
  const retrying = retryingError !== null && retryingError === error
  const Heading = headingLevel === 2 ? 'h2' : 'h1'

  useEffect(() => headingRef.current?.focus(), [])

  return (
    <section
      className="state-panel state-error"
      role="alert"
      aria-live="assertive"
      aria-atomic="true"
      aria-labelledby={headingID}
    >
      <Heading id={headingID} ref={headingRef} tabIndex={-1}>
        {title}
      </Heading>
      <p>{failure.message}</p>
      <div className="state-actions">
        {failure.retryable && onRetry && (
          <button
            type="button"
            disabled={retrying}
            onClick={() => {
              setRetryingError(error)
              onRetry()
            }}
          >
            {retrying ? 'Retrying…' : 'Retry'}
          </button>
        )}
        {backHref && backLabel && (
          <a className="secondary-button" href={backHref}>
            {backLabel}
          </a>
        )}
      </div>
    </section>
  )
}
