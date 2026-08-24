import { useSettingsMutation } from '../settingsMutation'

export default function ConfirmButton({
  label,
  resource,
  consequence,
  disabled = false,
  title,
  onConfirm,
}: {
  label: string
  resource: string
  consequence: string
  disabled?: boolean
  title?: string
  onConfirm: () => void
}) {
  const { confirm } = useSettingsMutation()

  return (
    <button
      type="button"
      className="secondary-button inline-confirm"
      disabled={disabled}
      title={title}
      onClick={(event) =>
        confirm({
          action: label,
          resource,
          consequence,
          confirmLabel: label,
          onConfirm,
          trigger: event.currentTarget,
        })
      }
    >
      {label}
    </button>
  )
}
