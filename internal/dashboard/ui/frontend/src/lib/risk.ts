import type { AttentionState, RiskState } from './api'

export interface RiskStateMeta {
  state: RiskState
  label: string
  /** What puts an identity in the state, in a few words. */
  detail: string
  /** How a count of identities in the state reads when it is not zero. */
  tone: 'danger' | 'warn' | 'positive'
  cls: string
  /** The Workloads list filtered to the state. */
  to: string
}

// In the backend's precedence (riskStateOf), so lists read most actionable first.
const byState = {
  conflicted: { label: 'Conflicted', detail: 'policies disagree', tone: 'danger' },
  blocked: { label: 'Blocked', detail: 'last apply failed', tone: 'danger' },
  'at-risk': { label: 'At risk', detail: 'OOM in 24h', tone: 'danger' },
  drifted: { label: 'Drifted', detail: 'pods not on rec', tone: 'warn' },
  safe: { label: 'Safe', detail: 'nothing to act on', tone: 'positive' },
} as const satisfies Record<RiskState, Pick<RiskStateMeta, 'label' | 'detail' | 'tone'>>

export const riskStateMeta: readonly RiskStateMeta[] = (Object.keys(byState) as RiskState[]).map(
  (state) => ({
    state,
    ...byState[state],
    cls: `risk-${state}`,
    to: `/workloads?risk=${state}`,
  }),
)

export const attentionStateMeta = riskStateMeta.filter(
  (m): m is RiskStateMeta & { state: AttentionState } => m.state !== 'safe',
)

export function riskStateMetaOf(state: string | undefined): RiskStateMeta | undefined {
  return riskStateMeta.find((m) => m.state === state)
}
