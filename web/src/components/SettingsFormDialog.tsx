import { type ReactNode, useEffect, useId, useRef } from 'react'

// Controlled native <dialog> for the settings inventories' Add/Edit forms.
// The panel owns the open flag (typically `draft !== null`) so a route-guard
// discard or a conflict reload closes the dialog by clearing the draft, and
// the platform supplies modal focus, Esc, backdrop, and focus restoration —
// no overlay machinery. Escape is swallowed while a save is in flight so the
// request cannot complete into a closed dialog.
export default function SettingsFormDialog({
  open,
  title,
  busy = false,
  size = 'default',
  onClose,
  children,
}: {
  open: boolean
  title: string
  busy?: boolean
  // 'compact' for a form with a field or two; the default fits three
  // grid columns.
  size?: 'default' | 'compact'
  onClose: () => void
  children: ReactNode
}) {
  const dialogRef = useRef<HTMLDialogElement>(null)
  const titleID = useId()

  useEffect(() => {
    const dialog = dialogRef.current
    if (!dialog) return
    if (open && !dialog.open) dialog.showModal()
    else if (!open && dialog.open) dialog.close()
  }, [open])

  // A confirm stacked above this dialog (the conflict reload) can unmount
  // the control that opened it, so closing the confirm drops focus on the
  // inert page instead of back inside. Pull it back to the first control.
  useEffect(() => {
    if (!open) return
    const recover = () =>
      requestAnimationFrame(() => {
        const dialog = dialogRef.current
        if (!dialog?.open || document.activeElement !== document.body) return
        dialog.querySelector<HTMLElement>('input:enabled, select:enabled, textarea:enabled, button:enabled')?.focus()
      })
    document.addEventListener('focusout', recover)
    return () => document.removeEventListener('focusout', recover)
  }, [open])

  return (
    <dialog
      ref={dialogRef}
      className={
        size === 'compact'
          ? 'users-dialog settings-form-dialog settings-form-dialog-compact'
          : 'users-dialog settings-form-dialog'
      }
      aria-labelledby={titleID}
      onCancel={(event) => {
        event.preventDefault()
        if (!busy) onClose()
      }}
    >
      {open && (
        <>
          <h2 id={titleID}>{title}</h2>
          {children}
        </>
      )}
    </dialog>
  )
}
