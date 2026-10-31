import type { RecommendationOutcome } from './api'

const labels: Record<RecommendationOutcome, string> = {
  Computed: 'Computed',
  NoData: 'No data',
  TooYoung: 'Too young',
  FetchFailed: 'Fetch failed',
  Conflicted: 'Conflicted (frozen)',
}

export function outcomeLabel(outcome: RecommendationOutcome): string {
  return labels[outcome] ?? outcome
}
