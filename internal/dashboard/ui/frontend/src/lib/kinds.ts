import { api } from './api'

/** A workload kind a Policy can manage, as `/api/kinds` serves it. */
export interface WorkloadKind {
  name: string
  /** The `spec.rightSizing.update.types` field that manages the kind. */
  updateField: string
  /** Its pods are never evicted: under Ongoing they are only resized in place. */
  inPlaceOnly: boolean
}

let served: Promise<WorkloadKind[]> | undefined

/**
 * The kinds `/api/kinds` serves. They change only with the server binary, so
 * every view shares one fetch; a failed fetch is not kept, so the next call
 * retries.
 */
export function getKinds(): Promise<WorkloadKind[]> {
  served ??= api<WorkloadKind[]>('/api/kinds').catch((e: unknown) => {
    served = undefined
    throw e
  })
  return served
}

/**
 * A Policy's update mode per kind it manages, in the server's kind order.
 * Without the kinds it names each by its raw `update.types` key.
 */
export function updateModes(kinds: WorkloadKind[] | null, types?: Record<string, string>): string {
  const modes = kinds
    ? kinds.flatMap((k) => {
        const mode = types?.[k.updateField]
        if (!mode) return []
        return [`${k.name}: ${mode}${k.inPlaceOnly && mode === 'Ongoing' ? ' (in place)' : ''}`]
      })
    : Object.entries(types ?? {}).flatMap(([field, mode]) => (mode ? [`${field}: ${mode}`] : []))
  return modes.join(', ') || '-'
}
