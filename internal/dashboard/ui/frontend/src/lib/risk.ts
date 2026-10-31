import type { AttentionState, RiskState } from './api'

export interface RiskStateMeta {
  state: RiskState
  label: string
  cls: string
  workloadsLink: string
}

export interface AttentionStateMeta extends RiskStateMeta {
  state: AttentionState
  /** What puts an identity in the state, in a few words. */
  detail: string
  /** How a count of identities in the state reads when it is not zero. */
  tone: 'danger' | 'warn'
}

// In the backend's precedence (riskStateOf), so lists read most actionable first.
const attention = {
  conflicted: { label: 'Conflicted', detail: 'policies disagree', tone: 'danger' },
  blocked: { label: 'Blocked', detail: 'last apply failed', tone: 'danger' },
  'at-risk': { label: 'At risk', detail: 'OOM in 24h', tone: 'danger' },
  drifted: { label: 'Drifted', detail: 'pods not on rec', tone: 'warn' },
} as const satisfies Record<AttentionState, Pick<AttentionStateMeta, 'label' | 'detail' | 'tone'>>

function derivedFrom<S extends RiskState>(state: S) {
  return { state, cls: `risk-${state}`, workloadsLink: `/workloads?risk=${state}` }
}

export function attentionStateMetaOf(state: AttentionState): AttentionStateMeta {
  return { ...derivedFrom(state), ...attention[state] }
}

export const attentionStateMeta: readonly AttentionStateMeta[] = (
  Object.keys(attention) as AttentionState[]
).map(attentionStateMetaOf)

export const riskStateMeta: readonly RiskStateMeta[] = [
  ...attentionStateMeta,
  { ...derivedFrom('safe'), label: 'Safe' },
]

export function riskStateMetaOf(state: string | undefined): RiskStateMeta | undefined {
  return riskStateMeta.find((m) => m.state === state)
}
