import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useId,
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
  // Returns the action that opens the confirmed reload, so an editor
  // inside a modal <dialog> (where the toast is inert) can offer it too.
  conflict: (label: string, reload: () => void) => () => void
  clearNotifications: () => void
  guardAction: (action: () => void) => void
}

const SettingsMutationContext = createContext<SettingsMutationContextValue | null>(null)

export function SettingsMutationProvider({ children }: { children: ReactNode }) {
  const dirtyForms = useRef(new Map<string, DirtyForm>())
  const [hasDirty, setHasDirty] = useState(false)
  const [toasts, setToasts] = useState<Toast[]>([])
  const [confirmation, setConfirmation] = useState<ConfirmationRequest | null>(null)
  const [pendingRoute, setPendingRoute] = useState<{ hash: string; mode: HistoryMode } | null>(null)
  const pendingRouteRef = useRef<{ hash: string; mode: HistoryMode } | null>(null)
  const confirmationRef = useRef<ConfirmationRequest | null>(null)
  const toastID = useRef(0)
  const dialogRef = useRef<HTMLDialogElement>(null)
  const cancelRef = useRef<HTMLButtonElement>(null)
  const confirmTitleID = useId()
  const acceptedHash = useRef(typeof location === 'undefined' ? '#/' : canonicalizeRouteHash(location.hash).hash)

  const registerDirty = useCallback((id: string, form: DirtyForm | null) => {
    if (form) dirtyForms.current.set(id, form)
    else dirtyForms.current.delete(id)
    setHasDirty(dirtyForms.current.size > 0)
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
      const askReload = () =>
        confirm({
          action: 'Reload server version',
          resource: label,
          consequence: 'This discards your local edits and replaces them with the latest server version.',
          confirmLabel: 'Reload',
          onConfirm: reload,
        })
      error(`${label} changed on the server. Your changes were not saved.`, {
        label: 'Reload server version',
        run: askReload,
      })
      return askReload
    },
    [confirm, error],
  )

  const discardAll = useCallback(() => {
    for (const form of dirtyForms.current.values()) form.discard()
    dirtyForms.current.clear()
    setHasDirty(false)
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
    (hash: string, mode: HistoryMode, fromHash = location.hash || acceptedHash.current) => {
      const canonical = canonicalizeRouteHash(hash).hash
      if (dirtyForms.current.size === 0 || !routeChangeDiscardsSettingsDraft(fromHash, canonical)) {
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
      if (blockRoute(next, 'push', acceptedHash.current)) return
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
  }, [hasDirty])

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
        aria-labelledby={confirmTitleID}
        onCancel={(event) => {
          event.preventDefault()
          closeConfirmation()
        }}
      >
        {confirmation && (
          <>
            <h2 id={confirmTitleID}>{confirmation.action}</h2>
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
  useLayoutEffect(() => {
    discardRef.current = discard
  }, [discard])
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
  const [snapshot, setSnapshot] = useState({ baseline: loaded, editing })
  const baseline = synchronizeDraftBaseline(snapshot.baseline, loaded, editing, snapshot.editing)
  if (snapshot.editing !== editing || serverSnapshotChanged(snapshot.baseline, baseline)) {
    setSnapshot({ baseline, editing })
  }
  const dirty = editing && current !== null && serverSnapshotChanged(baseline, current)
  const release = useSettingsDraft(id, label, dirty, discard)
  // The last preflight conflict, for editors that render inside a modal
  // <dialog>: the provider's toast (and its reload action) sits outside
  // the dialog and is inert while it is open, so the editor shows the
  // notice and the same confirmed reload itself. Cleared when the editor
  // closes, when a later preflight passes, and when the reload is applied.
  const [conflict, setConflict] = useState<{ message: string; reload: () => void } | null>(null)
  if (!editing && conflict !== null) setConflict(null)

  const checkForConflict = useCallback(
    async (fetchLatest: () => Promise<T>): Promise<boolean> => {
      const latest = await fetchLatest()
      if (!serverSnapshotChanged(baseline, latest)) {
        setConflict(null)
        return true
      }
      const askReload = feedback.conflict(label, () => {
        setSnapshot({ baseline: latest, editing })
        setConflict(null)
        reload(latest)
      })
      setConflict({ message: `${label} changed on the server. Your changes were not saved.`, reload: askReload })
      return false
    },
    [baseline, editing, feedback, label, reload],
  )

  return { dirty, checkForConflict, release, conflict }
}
