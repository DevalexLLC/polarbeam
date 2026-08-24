import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useLayoutEffect,
  useMemo,
  useRef,
  useState,
  type ReactNode,
} from 'react'
import {
  canonicalizeRouteHash,
  navigateRouteHash,
  routeChangeDiscardsSettingsDraft,
  setRouteNavigationBlocker,
  type HistoryMode,
} from './routeState'
import { serverSnapshotChanged, synchronizeDraftBaseline } from './settingsSnapshot'

const SUCCESS_MS = 4_000

interface DirtyForm {
  label: string
  consequence: string
  discard: () => void
}

interface Toast {
  id: number
  kind: 'success' | 'error'
  message: string
  actionLabel?: string
  onAction?: () => void
}

export interface ConfirmationRequest {
  action: string
  resource: string
  consequence: string
  confirmLabel?: string
  cancelLabel?: string
  onConfirm: () => void
  trigger?: HTMLElement | null
}

interface SettingsMutationContextValue {
  registerDirty: (id: string, form: DirtyForm | null) => void
  success: (message: string) => void
  error: (message: string, action?: { label: string; run: () => void }) => void
  confirm: (request: ConfirmationRequest) => void
  conflict: (label: string, reload: () => void) => void
  clearNotifications: () => void
  guardAction: (action: () => void) => void
}

const SettingsMutationContext = createContext<SettingsMutationContextValue | null>(null)

export function SettingsMutationProvider({ children }: { children: ReactNode }) {
  const dirtyForms = useRef(new Map<string, DirtyForm>())
  const [dirtyVersion, setDirtyVersion] = useState(0)
  const [toasts, setToasts] = useState<Toast[]>([])
  const [confirmation, setConfirmation] = useState<ConfirmationRequest | null>(null)
  const [pendingRoute, setPendingRoute] = useState<{ hash: string; mode: HistoryMode } | null>(null)
  const pendingRouteRef = useRef<{ hash: string; mode: HistoryMode } | null>(null)
  const confirmationRef = useRef<ConfirmationRequest | null>(null)
  const toastID = useRef(0)
  const dialogRef = useRef<HTMLDialogElement>(null)
  const cancelRef = useRef<HTMLButtonElement>(null)
  const acceptedHash = useRef(typeof location === 'undefined' ? '#/' : canonicalizeRouteHash(location.hash).hash)
  const hasDirty = dirtyForms.current.size > 0

  const registerDirty = useCallback((id: string, form: DirtyForm | null) => {
    const previous = dirtyForms.current.get(id)
    if (form) dirtyForms.current.set(id, form)
    else dirtyForms.current.delete(id)
    if (previous !== form) setDirtyVersion((version) => version + 1)
  }, [])

  const dismiss = useCallback((id: number) => {
    setToasts((current) => current.filter((toast) => toast.id !== id))
  }, [])

  const success = useCallback(
    (message: string) => {
      const id = ++toastID.current
      setToasts((current) => [...current, { id, kind: 'success', message }])
      window.setTimeout(() => dismiss(id), SUCCESS_MS)
    },
    [dismiss],
  )

  const error = useCallback((message: string, action?: { label: string; run: () => void }) => {
    const id = ++toastID.current
    setToasts((current) => [
      ...current,
      { id, kind: 'error', message, actionLabel: action?.label, onAction: action?.run },
    ])
  }, [])
  const clearNotifications = useCallback(() => setToasts([]), [])

  const confirm = useCallback((request: ConfirmationRequest) => {
    if (confirmationRef.current) return
    confirmationRef.current = request
    setConfirmation(request)
  }, [])
  const conflict = useCallback(
    (label: string, reload: () => void) => {
      error(`${label} changed on the server. Your changes were not saved.`, {
        label: 'Reload server version',
        run: () =>
          confirm({
            action: 'Reload server version',
            resource: label,
            consequence: 'This discards your local edits and replaces them with the latest server version.',
            confirmLabel: 'Reload',
            onConfirm: reload,
          }),
      })
    },
    [confirm, error],
  )

  const discardAll = useCallback(() => {
    for (const form of dirtyForms.current.values()) form.discard()
    dirtyForms.current.clear()
    setDirtyVersion((version) => version + 1)
  }, [])

  const discardDescription = useCallback(() => {
    const forms = [...dirtyForms.current.values()]
    const consequences = [...new Set(forms.map((form) => form.consequence))]
    return {
      resource: forms.length === 1 ? forms[0].label : `${forms.length} unsaved Settings forms`,
      consequence: consequences.join(' '),
    }
  }, [])

  const guardAction = useCallback(
    (action: () => void) => {
      if (dirtyForms.current.size === 0) {
        action()
        return
      }
      const description = discardDescription()
      confirm({
        action: 'Discard changes',
        ...description,
        confirmLabel: 'Discard',
        cancelLabel: 'Stay',
        onConfirm: () => {
          discardAll()
          action()
        },
      })
    },
    [confirm, discardAll, discardDescription],
  )

  const blockRoute = useCallback(
    (hash: string, mode: HistoryMode) => {
      const canonical = canonicalizeRouteHash(hash).hash
      if (
        dirtyForms.current.size === 0 ||
        !routeChangeDiscardsSettingsDraft(location.hash || acceptedHash.current, canonical)
      ) {
        acceptedHash.current = canonical
        return true
      }
      if (pendingRouteRef.current || confirmationRef.current) return false
      const pending = { hash: canonical, mode }
      const description = discardDescription()
      pendingRouteRef.current = pending
      setPendingRoute(pending)
      confirm({
        action: 'Discard changes',
        ...description,
        confirmLabel: 'Discard',
        cancelLabel: 'Stay',
        onConfirm: discardAll,
      })
      return false
    },
    [confirm, discardAll, discardDescription],
  )

  useEffect(() => {
    setRouteNavigationBlocker(blockRoute)
    return () => setRouteNavigationBlocker(null)
  }, [blockRoute])

  // Install ahead of App's passive route subscription. Back/Forward changes
  // the hash before its event is observable, so restore the accepted URL
  // while the operator chooses Stay or Discard.
  useLayoutEffect(() => {
    const onBrowserRoute = (event: Event) => {
      const next = canonicalizeRouteHash(location.hash).hash
      if (next === acceptedHash.current) return
      if (blockRoute(next, 'push')) return
      history.pushState(null, '', acceptedHash.current)
      event.stopImmediatePropagation()
    }
    const onAnchor = (event: MouseEvent) => {
      if (
        event.defaultPrevented ||
        event.button !== 0 ||
        event.metaKey ||
        event.ctrlKey ||
        event.shiftKey ||
        event.altKey
      )
        return
      const target = event.target instanceof Element ? event.target.closest<HTMLAnchorElement>('a[href^="#/"]') : null
      if (!target || dirtyForms.current.size === 0) return
      if (!blockRoute(target.hash, 'push')) event.preventDefault()
    }
    window.addEventListener('hashchange', onBrowserRoute)
    window.addEventListener('popstate', onBrowserRoute)
    document.addEventListener('click', onAnchor, true)
    return () => {
      window.removeEventListener('hashchange', onBrowserRoute)
      window.removeEventListener('popstate', onBrowserRoute)
      document.removeEventListener('click', onAnchor, true)
    }
  }, [blockRoute])

  useEffect(() => {
    if (!hasDirty) return
    const beforeUnload = (event: BeforeUnloadEvent) => {
      event.preventDefault()
      event.returnValue = ''
    }
    window.addEventListener('beforeunload', beforeUnload)
    return () => window.removeEventListener('beforeunload', beforeUnload)
  }, [dirtyVersion, hasDirty])

  useEffect(() => {
    const dialog = dialogRef.current
    if (!dialog || !confirmation) return
    if (!dialog.open) dialog.showModal()
    cancelRef.current?.focus()
  }, [confirmation])

  const closeConfirmation = useCallback(() => {
    dialogRef.current?.close()
    const trigger = confirmation?.trigger
    confirmationRef.current = null
    pendingRouteRef.current = null
    setConfirmation(null)
    setPendingRoute(null)
    requestAnimationFrame(() => trigger?.focus())
  }, [confirmation])

  const acceptConfirmation = useCallback(() => {
    const request = confirmation
    if (!request) return
    dialogRef.current?.close()
    confirmationRef.current = null
    setConfirmation(null)
    request.onConfirm()
    if (pendingRoute) {
      const destination = pendingRoute
      pendingRouteRef.current = null
      setPendingRoute(null)
      const canonical = canonicalizeRouteHash(destination.hash).hash
      acceptedHash.current = canonical
      navigateRouteHash(canonical, destination.mode, true)
    } else {
      requestAnimationFrame(() => request.trigger?.focus())
    }
  }, [confirmation, pendingRoute])

  const value = useMemo(
    () => ({ registerDirty, success, error, confirm, conflict, clearNotifications, guardAction }),
    [clearNotifications, confirm, conflict, error, guardAction, registerDirty, success],
  )

  return (
    <SettingsMutationContext.Provider value={value}>
      {children}
      <div className="settings-toasts" aria-label="Settings notifications" aria-live="polite" aria-atomic="false">
        {toasts.map((toast) => (
          <div
            key={toast.id}
            className={`settings-toast settings-toast-${toast.kind}`}
            role={toast.kind === 'error' ? 'alert' : 'status'}
          >
            <span>{toast.message}</span>
            {toast.actionLabel && toast.onAction && (
              <button type="button" className="linklike" onClick={toast.onAction}>
                {toast.actionLabel}
              </button>
            )}
            <button
              type="button"
              className="toast-dismiss"
              aria-label="Dismiss notification"
              onClick={() => dismiss(toast.id)}
            >
              ×
            </button>
          </div>
        ))}
      </div>
      <dialog
        ref={dialogRef}
        className="users-dialog settings-confirm-dialog"
        onCancel={(event) => {
          event.preventDefault()
          closeConfirmation()
        }}
      >
        {confirmation && (
          <>
            <h2>{confirmation.action}</h2>
            <p>
              <strong>{confirmation.resource}</strong>
            </p>
            <p className="section-intro">{confirmation.consequence}</p>
            <div className="users-dialog-foot">
              <button ref={cancelRef} type="button" className="secondary-button" onClick={closeConfirmation}>
                {confirmation.cancelLabel ?? 'Cancel'}
              </button>
              <button type="button" className="danger-button" onClick={acceptConfirmation}>
                {confirmation.confirmLabel ?? confirmation.action}
              </button>
            </div>
          </>
        )}
      </dialog>
    </SettingsMutationContext.Provider>
  )
}

export function useSettingsMutation(): SettingsMutationContextValue {
  const context = useContext(SettingsMutationContext)
  if (!context) throw new Error('useSettingsMutation must be used inside SettingsMutationProvider')
  return context
}

export function useSettingsDraft(
  id: string,
  label: string,
  dirty: boolean,
  discard: () => void,
  consequence = 'Your local edits will be discarded before leaving this page.',
): () => void {
  const { registerDirty } = useSettingsMutation()
  const discardRef = useRef(discard)
  discardRef.current = discard
  useEffect(() => {
    registerDirty(id, dirty ? { label, consequence, discard: () => discardRef.current() } : null)
    return () => registerDirty(id, null)
  }, [consequence, dirty, id, label, registerDirty])
  return useCallback(() => registerDirty(id, null), [id, registerDirty])
}

export function useConcurrentSettingsDraft<T>({
  id,
  label,
  loaded,
  current,
  editing,
  discard,
  reload,
}: {
  id: string
  label: string
  loaded: T | null
  current: T | null
  editing: boolean
  discard: () => void
  reload: (latest: T) => void
}) {
  const feedback = useSettingsMutation()
  const baseline = useRef<T | null>(loaded)
  const wasEditing = useRef(editing)
  baseline.current = synchronizeDraftBaseline(baseline.current, loaded, editing, wasEditing.current)
  wasEditing.current = editing
  const dirty = editing && current !== null && serverSnapshotChanged(baseline.current, current)
  const release = useSettingsDraft(id, label, dirty, discard)

  const checkForConflict = useCallback(
    async (fetchLatest: () => Promise<T>): Promise<boolean> => {
      const latest = await fetchLatest()
      if (!serverSnapshotChanged(baseline.current, latest)) return true
      feedback.conflict(label, () => {
        baseline.current = latest
        reload(latest)
      })
      return false
    },
    [feedback, label, reload],
  )

  return { dirty, checkForConflict, release }
}
