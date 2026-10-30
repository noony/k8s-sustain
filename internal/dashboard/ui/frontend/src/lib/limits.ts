import type { MergePatch, Resource, ResourceLimitsConfig } from './api'

/** A limit strategy: the one ResourceLimitsConfig field a Policy sets. */
export type LimitMode = keyof ResourceLimitsConfig

export interface LimitModeMeta {
  label: string
  /** How a trace names the stage the strategy runs as. */
  stage: string
  help: Record<Resource, string>
}

const both = (text: string): Record<Resource, string> => ({ cpu: text, memory: text })

// In the recommender's precedence, so the first one a config sets is the one it runs.
export const limitModes = {
  keepLimit: {
    label: 'Keep existing limit',
    stage: 'Limit kept',
    help: both('Existing pod limits stay unchanged.'),
  },
  noLimit: {
    label: 'Remove limit',
    stage: 'Limit removed',
    help: {
      cpu: 'Strips the CPU limit from the pod.',
      memory: 'Strips the memory limit from the pod.',
    },
  },
  equalsToRequest: {
    label: 'Equal to request',
    stage: 'Limit equal to request',
    help: {
      cpu: 'Limit = request. Note: CPU is throttled at the request value.',
      memory: 'Limit = request. Memory above the request value triggers OOM-kill.',
    },
  },
  requestsLimitsRatio: {
    label: 'Multiple of request',
    stage: 'Limit at a ratio of the request',
    help: both('Limit = request × ratio.'),
  },
  keepLimitRequestRatio: {
    label: 'Preserve current limit/request ratio',
    stage: 'Limit at the kept ratio',
    help: both('Limit = request × (current limit ÷ current request), per container.'),
  },
} satisfies Record<LimitMode, LimitModeMeta>

export const limitModeOrder = Object.keys(limitModes) as LimitMode[]

export function limitModeOf(limits: ResourceLimitsConfig | undefined): LimitMode {
  return limitModeOrder.find((m) => limits?.[m] != null && limits[m] !== false) ?? 'keepLimit'
}

// A merge patch keeps the Policy's own strategy unless it is turned off, and
// the CRD admits only one.
export function limitsPatch(mode: LimitMode, ratio: number): MergePatch<ResourceLimitsConfig> {
  const patch: MergePatch<ResourceLimitsConfig> = {
    requestsLimitsRatio: mode === 'requestsLimitsRatio' ? ratio : null,
  }
  for (const m of limitModeOrder) {
    if (m !== 'requestsLimitsRatio') patch[m] = m === mode
  }
  return patch
}
