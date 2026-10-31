import { api } from './api'

/** A workload kind a Policy can manage, as `/api/kinds` serves it. */
export interface WorkloadKind {
  name: string
  /** The `spec.rightSizing.update.types` field that manages the kind. */
  updateField: string
  /** Its pods are never evicted: under Ongoing they are only resized in place. */
  inPlaceOnly: boolean
}

export function fetchKinds(): Promise<WorkloadKind[]> {
  return api<WorkloadKind[]>('/api/kinds')
}

/** A Policy's update mode per kind it manages, in the server's kind order. */
export function updateModes(kinds: WorkloadKind[], types?: Record<string, string>): string {
  const modes = kinds.flatMap((k) => {
    const mode = types?.[k.updateField]
    if (!mode) return []
    return [`${k.name}: ${mode}${k.inPlaceOnly && mode === 'Ongoing' ? ' (in place)' : ''}`]
  })
  return modes.join(', ') || '-'
}
