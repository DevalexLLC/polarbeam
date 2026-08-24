export default function DisclosureChevron({ expanded }: { expanded: boolean }) {
  return (
    <svg
      className={'disclosure-chevron' + (expanded ? ' disclosure-chevron-expanded' : '')}
      viewBox="0 0 20 20"
      aria-hidden="true"
    >
      <path d="m7 4 6 6-6 6" />
    </svg>
  )
}
