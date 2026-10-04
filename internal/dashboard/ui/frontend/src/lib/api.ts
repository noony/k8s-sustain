const API_BASE = ''

// ApiError carries the structured error returned by the dashboard's API.
// Callers may catch and inspect `code` / `field` / `requestId` for richer
// UX; ones that only need a string get `.message` via Error.
export class ApiError extends Error {
  code: string
  status: number
  field?: string
  requestId?: string

  constructor(
    message: string,
    opts: { code: string; status: number; field?: string; requestId?: string },
  ) {
    super(message)
    this.name = 'ApiError'
    this.code = opts.code
    this.status = opts.status
    this.field = opts.field
    this.requestId = opts.requestId
  }
}

// Every dashboard response is wrapped as `{data, meta}` on success or `{error}`
// on failure; api() unwraps that envelope and throws ApiError on HTTP errors.
export async function api<T = unknown>(path: string, opts?: RequestInit): Promise<T> {
  const res = await fetch(API_BASE + path, opts)
  const requestId = res.headers.get('X-Request-Id') || undefined
  if (!res.ok) {
    const body = await res.json().catch(() => null)
    const err = body?.error ?? {}
    throw new ApiError(err.message || body?.error || res.statusText, {
      code: err.code || 'INTERNAL',
      status: res.status,
      field: err.field,
      requestId: err.requestId || requestId,
    })
  }
  const body = (await res.json()) as { data?: T } & T
  // Tolerate non-enveloped responses (UI development against an older server).
  return (body && typeof body === 'object' && 'data' in body ? (body.data as T) : body) as T
}

export interface TimeRangeOption {
  label: string
  window: string
  step: string
}

export const timeRangeOptions: TimeRangeOption[] = [
  { label: '1h', window: '1h', step: '1m' },
  { label: '4h', window: '4h', step: '1m' },
  { label: '12h', window: '12h', step: '5m' },
  { label: '1d', window: '24h', step: '5m' },
  { label: '3d', window: '72h', step: '5m' },
  { label: '7d', window: '168h', step: '10m' },
  { label: '30d', window: '720h', step: '20m' },
]

export interface Condition {
  type: string
  status: string
  reason?: string
}

export interface PolicySummary {
  name: string
  namespaces?: string[]
  conditions?: Condition[]
  update?: Record<string, string>
  createdAt?: string
  workloadCount?: number
  cpuSavingsCores?: number
  memSavingsBytes?: number
  blockedCount?: number
  lastAppliedAt?: string
}

export interface LabelSelector {
  matchLabels?: Record<string, string>
  matchExpressions?: {
    key: string
    operator: string
    values?: string[]
  }[]
}

export interface PolicySelector {
  namespaces?: string[]
  labelSelector?: LabelSelector
}

export interface UpdateTypes {
  deployment?: string
  statefulSet?: string
  daemonSet?: string
  cronJob?: string
  job?: string
  argoRollout?: string
}

export interface EvictionPolicy {
  ignoreAutoscalerSafeToEvictAnnotations?: boolean
}

export interface AutoscalerCoordination {
  enabled?: boolean
  replicaBudgetAnchor?: number
}

export interface PolicySpec {
  name?: string
  spec?: {
    selector?: PolicySelector
    rightSizing?: {
      resourcesConfigs?: {
        cpu?: ResourceConfig
        memory?: ResourceConfig
      }
      update?: {
        types?: UpdateTypes
        eviction?: EvictionPolicy
      }
      autoscalerCoordination?: AutoscalerCoordination
      excludeInitContainers?: boolean
    }
  }
  // top-level update map is also exposed by the backend (flat snapshot of spec.rightSizing.update.types)
  update?: UpdateTypes
  conditions?: Condition[]
  workloadCount?: number
  cpuSavingsCores?: number
  memSavingsBytes?: number
  blockedCount?: number
  effectivenessSeries?: { cpu: TimeValue[]; memory: TimeValue[] }
}

export interface ResourceLimitsConfig {
  equalsToRequest?: boolean
  keepLimit?: boolean
  keepLimitRequestRatio?: boolean
  noLimit?: boolean
  requestsLimitsRatio?: number
}

export interface ResourceConfig {
  window?: string
  requests?: {
    percentile?: number
    headroom?: number
    minAllowed?: string
    maxAllowed?: string
    keepRequest?: boolean
  }
  limits?: ResourceLimitsConfig
}

export interface ContainerInfo {
  name: string
  cpuRequest?: string
  memoryRequest?: string
}

export interface WorkloadItem {
  namespace: string
  kind: string
  name: string
  automated?: boolean
  policyName?: string
  containers: ContainerInfo[]
}

export interface OverviewData {
  totalWorkloads: number
  automated: number
  manual: number
  cpu: SavingsInfo
  memory: SavingsInfo
  workloads?: OverviewWorkload[]
}

export interface SavingsInfo {
  savingsMillis: number
  savingsFormatted: string
  currentFormatted: string
  recommendedFormatted: string
  savingsPercent?: number
}

export interface OverviewWorkload {
  namespace: string
  kind: string
  name: string
  policyName: string
  cpuDeltaPercent: number
  memDeltaPercent: number
}

export interface WorkloadListData {
  items: WorkloadItemV2[]
  total: number
  pageSize: number
  namespaces?: string[]
  kinds?: string[]
  counts: { total: number; automated: number; manual: number; conflicted?: number }
}

export interface PolicyWorkloadsData {
  items: WorkloadItemV2[]
  total: number
  matched?: number
  pageSize: number
  namespaces?: string[]
}

export interface TimeValue {
  timestamp: string
  value: number
}

export interface OOMEvent {
  timestamp: string
  container: string
  pod: string
}

export interface ContainerResources {
  cpuRequest?: string
  cpuLimit?: string
  memoryRequest?: string
  memoryLimit?: string
}

export interface MetricsData {
  cpu: Record<string, TimeValue[]>
  memory: Record<string, TimeValue[]>
  resources?: Record<string, ContainerResources>
  cpuRequests?: Record<string, TimeValue[]>
  memoryRequests?: Record<string, TimeValue[]>
  cpuLimits?: Record<string, TimeValue[]>
  memoryLimits?: Record<string, TimeValue[]>
  oomEvents?: OOMEvent[]
  initContainers?: string[]
}

export interface RecommendationContainer {
  cpuRequest?: string
  memoryRequest?: string
  cpuLimit?: string
  memoryLimit?: string
  cpuLimitRemoved?: boolean
  memoryLimitRemoved?: boolean
}

/** What the controller last decided for an identity, as its WorkloadRecommendation records it. */
export type RecommendationOutcome =
  'Computed' | 'NoData' | 'TooYoung' | 'FetchFailed' | 'Conflicted'

export interface OOMFloorTrace {
  value: string
  /** The floor, not the percentile or a min/max clamp, set the final request. */
  determined?: boolean
}

export interface CoordinationTrace {
  overheadFactor: number
  replicaFactor?: number
  scaled: string
  value: string
}

/** One resource's request after each stage that ran, as Kubernetes quantities. */
export interface ResourceTrace {
  percentile?: string
  oomFloor?: OOMFloorTrace
  withHeadroom: string
  clamped: string
  coordination?: CoordinationTrace
  limit?: string
  removeLimit?: boolean
}

export interface ContainerTrace {
  cpu?: ResourceTrace
  memory?: ResourceTrace
}

/** The Recommendation an identity's WorkloadRecommendation stores; never recomputed by the dashboard. */
export interface StoredRecommendation {
  outcome?: RecommendationOutcome
  observedAt?: string
  containers?: Record<string, RecommendationContainer>
  /** How each container's values were derived, stage by stage. */
  trace?: Record<string, ContainerTrace>
}

export interface SimulateRequest {
  namespace: string
  ownerKind: string
  ownerName: string
  window?: string
  fromTs?: number
  toTs?: number
  step: string
  cpu: SimulateResourceConfig
  memory: SimulateResourceConfig
  /** Overrides the managing policy's autoscaler coordination; omit to inherit it. */
  autoscalerCoordination?: SimulateAutoscalerCoordination
}

export interface SimulateAutoscalerCoordination {
  enabled?: boolean
  replicaBudgetAnchor?: number
}

export interface SimulateResourceConfig {
  percentile: number
  headroom: number
  window: string
  minAllowed?: string
  maxAllowed?: string
  limits?: SimulateLimitsConfig
}

export interface SimulateLimitsConfig {
  equalsToRequest?: boolean
  keepLimit?: boolean
  keepLimitRequestRatio?: boolean
  noLimit?: boolean
  requestsLimitsRatio?: number
}

export interface SimulationResult {
  containers: Record<string, RecommendationContainer>
  initContainers?: string[]
  tooYoung?: boolean
  cpuSeries: Record<string, TimeValue[]>
  memorySeries: Record<string, TimeValue[]>
  resources?: Record<string, ContainerResources>
  cpuRequests?: Record<string, TimeValue[]>
  memoryRequests?: Record<string, TimeValue[]>
  cpuRecommendations?: Record<string, TimeValue[]>
  memoryRecommendations?: Record<string, TimeValue[]>
}

export interface BatchSimulateData {
  cpu: SavingsInfo
  memory: SavingsInfo
  workloads: BatchWorkloadResult[]
}

export interface BatchWorkloadResult {
  namespace: string
  kind: string
  name: string
  error?: string
  containers?: Record<
    string,
    {
      currentCpu?: string
      recommendedCpu?: string
      currentMemory?: string
      recommendedMemory?: string
    }
  >
}

export interface SummaryV2 {
  kpi: {
    cpuSavedCores: number
    cpuSavedRatio: number
    cpuSpark7d: number[]
    memSavedBytes: number
    memSavedRatio: number
    memSpark7d: number[]
    atRiskCount: number
    blockedCount: number
    driftedCount: number
  }
  headroom: { cpu: HeadroomBreakdown; memory: HeadroomBreakdown }
  attention: {
    risk: AttentionRow[]
    drift: AttentionRow[]
    blocked: AttentionRow[]
  }
  policies: PolicyRollup[]
}

export interface HeadroomBreakdown {
  used: number
  idle: number
  free: number
}

export interface AttentionRow {
  namespace: string
  kind: string
  name: string
  policy?: string
  signal: string
  detail?: string
  lastSeen?: string
}

export interface PolicyRollup {
  name: string
  workloadCount: number
  cpuSavingsCores: number
  memSavingsBytes: number
  blockedCount: number
  lastAppliedAt?: string
}

export interface TrendSeries {
  usage: TimeValue[]
  request: TimeValue[]
  originalRequest: TimeValue[]
}

export interface TrendData {
  cpu: TrendSeries
  memory: TrendSeries
}

export interface ActivityItem {
  timestamp: string
  namespace: string
  kind: string
  name: string
  reason: string
  message: string
}

export interface CoordinationFactors {
  enabled: boolean
  cpuOverhead?: number
  memoryOverhead?: number
  cpuReplica?: number
}

export type RiskState = 'safe' | 'drifted' | 'at-risk' | 'blocked' | 'conflicted'

export interface WorkloadDetailSnapshot {
  automated: boolean
  policyName?: string
  /** The Policies a Conflicted identity's members opt into; no Policy governs it. */
  conflictingPolicies?: string[]
  departed: boolean
  updateMode?: string
  riskState: RiskState
  stalePods: number
  totalPods: number
  oom24h: number
  blocked?: { reason: string; attempts: number; nextRetryAt?: string; lastError?: string }
  recentEvents: ActivityItem[]
  coordinationFactors?: CoordinationFactors
  recommendation?: StoredRecommendation
}

export interface WorkloadItemV2 extends WorkloadItem {
  riskState: RiskState
  stalePods: number
  totalPods: number
  autoscalerPresent: boolean
  coordinationFactors?: CoordinationFactors
  departed: boolean
  lastSeenAt?: string
  conflictingPolicies?: string[]
}
