export function canonicalSnapshot(value: unknown): string {
  if (Array.isArray(value)) return `[${value.map(canonicalSnapshot).join(',')}]`
  if (value !== null && typeof value === 'object') {
    return `{${Object.entries(value)
      // ES2023 toSorted is outside this project's browser target. The
      // entries array is fresh, so sorting it cannot mutate caller state.
      // oxlint-disable-next-line unicorn/no-array-sort
      .sort(([a], [b]) => a.localeCompare(b))
      .map(([key, item]) => `${JSON.stringify(key)}:${canonicalSnapshot(item)}`)
      .join(',')}}`
  }
  return JSON.stringify(value)
}

export function serverSnapshotChanged(loaded: unknown, latest: unknown): boolean {
  return canonicalSnapshot(loaded) !== canonicalSnapshot(latest)
}

export function synchronizeDraftBaseline<T>(
  baseline: T | null,
  loaded: T | null,
  editing: boolean,
  wasEditing: boolean,
): T | null {
  return loaded !== null && (!editing || !wasEditing) ? loaded : baseline
}
