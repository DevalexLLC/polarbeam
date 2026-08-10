import { useTheme } from '../theme'

// The adaptive /polarbeam-mark.svg picks its palette with an embedded
// prefers-color-scheme query, which tracks the OS — not the manual theme
// toggle. In-app renders therefore use the static per-theme variants keyed
// off the resolved theme. The favicon keeps the adaptive mark: browser
// chrome follows the OS scheme, not data-theme.
export default function LogoMark({ className }: { className: string }) {
  const { resolved } = useTheme()
  return <img className={className} src={`/polarbeam-mark-${resolved}.svg`} alt="" />
}
