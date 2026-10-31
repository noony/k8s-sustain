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

/** One OOM kill, dated at the end of the chart step it fell in. */
export interface OOMKill {
  timestamp: string
  container: string
}

export interface ContainerResources {
  cpuRequest?: string
  cpuLimit?: string
  memoryRequest?: string
  memoryLimit?: string
}

export interface MetricsData {
  /** Usage averaged over the replicas, per container. */
  cpu: Record<string, TimeValue[]>
  memory: Record<string, TimeValue[]>
  resources?: Record<string, ContainerResources>
  cpuRequests?: Record<string, TimeValue[]>
  memoryRequests?: Record<string, TimeValue[]>
  cpuLimits?: Record<string, TimeValue[]>
  memoryLimits?: Record<string, TimeValue[]>
  oomKills?: OOMKill[]
  initContainers?: string[]
  /** What the usage signals read; null when no Policy governs the identity or it could not be read. */
  signal?: UsageSignal | null
  /** Why the signal could not be read; the rest of the page stands without it. */
  signalError?: string
}

/** What an identity's usage signals read over the chart range under its governing Policy's settings: what the recommender sizes each request from. */
export interface UsageSignal {
  policy: string
  /** The step between its instants: the server derives it from the range, the requested step is only the finest. */
  stepSeconds: number
  cpu: UsageSignalSettings
  memory: UsageSignalSettings
  /** The Policy recommends no init container, so none has a series. */
  excludeInitContainers?: boolean
  /** Per container the Policy recommends, in cores or bytes. */
  containers: Record<string, { cpu?: TimeValue[]; memory?: TimeValue[] }>
}

/** What one resource's usage signal read under, every default explicit. */
export interface UsageSignalSettings {
  /** Absent when the request is kept. */
  percentile?: number
  window: string
  /** The Policy keeps the request: no signal sizes it, so it has no series. */
  keepRequest?: boolean
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

/** One stage of a resource's computation. */
export interface TraceStage {
  stage: string
  /** What a signal contributed, or the request or limit after the stage, as a Kubernetes quantity. */
  value?: string
  /** The stage strips the limit. */
  removed?: boolean
  /** The stage's own details, shown as they are. */
  facts?: Record<string, string>
}

/** One resource's stages in the order they ran: the request's, then the limit's. */
export interface ResourceTrace {
  request?: TraceStage[]
  limit?: TraceStage[]
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

/** The settings of a Policy's right-sizing spec that shape a Recommendation. */
export interface SimulationSettings {
  resourcesConfigs: { cpu?: ResourceConfig; memory?: ResourceConfig }
  autoscalerCoordination: AutoscalerCoordination
  excludeInitContainers?: boolean
}

export type Resource = keyof SimulationSettings['resourcesConfigs']

/** A JSON merge patch (RFC 7386): a key set replaces the base's, null removes it. */
export type MergePatch<T> = {
  [K in keyof T]?: NonNullable<T[K]> extends object
    ? NonNullable<T[K]> extends unknown[]
      ? T[K] | null
      : MergePatch<NonNullable<T[K]>> | null
    : T[K] | null
}

export type SimulationOverride = MergePatch<SimulationSettings>

export interface SimulateRequest {
  namespace: string
  ownerKind: string
  ownerName: string
  /** The chart range; it does not change the recommendation windows. */
  window?: string
  fromTs?: number
  toTs?: number
  /** The finest step the history may use; the server coarsens it to fit the range. */
  step: string
  /** The Policy to start from; omit for the identity's governing Policy. */
  policy?: string
  /** Merged onto the starting Policy's settings; omit to simulate what the controller does. */
  override?: SimulationOverride
}

/** What a Simulation's computation can decide; a failed fetch is an error, and Conflicted the controller's alone. */
export type SimulationOutcome = Exclude<RecommendationOutcome, 'FetchFailed' | 'Conflicted'>

export interface SimulationResult {
  /** The Policy the Simulation started from; absent when none governs the identity and none was chosen. */
  policy?: string
  /** What it ran with: the Policy's settings, the override merged in, every default explicit. */
  effective: SimulationSettings
  outcome: SimulationOutcome
  containers: Record<string, RecommendationContainer>
  /** How each container's values were derived, stage by stage. */
  trace?: Record<string, ContainerTrace>
  initContainers?: string[]
  resources?: Record<string, ContainerResources>
  /** The Simulation at each instant of its chart range; absent when it could not be read. */
  history?: SimulationHistory
  /** Why the history could not be read; the numbers above stand without it. */
  historyError?: string
  /** How long a kill the controller's OOM watcher saw counts, which a Simulation cannot see. */
  liveOOMWindowSeconds: number
}

/** A Simulation at each instant of its chart range. */
export interface SimulationHistory {
  /** The step between its instants: the server derives it from the range, the requested step is only the finest. */
  stepSeconds: number
  containers: Record<string, { cpu: ResourceHistory; memory: ResourceHistory }>
  oomKills?: OOMKill[]
}

/** One resource of one container at each instant, in cores or bytes; a series has no point where it has no value. */
export interface ResourceHistory {
  /** What the usage signal read: the busiest replica's percentile over the Policy's window. */
  usage?: TimeValue[]
  /** What the Simulation computes as of each instant; no limit point where the container would run without one. */
  recommendedRequest?: TimeValue[]
  recommendedLimit?: TimeValue[]
  /** What the pods ran with. */
  runningRequest?: TimeValue[]
  runningLimit?: TimeValue[]
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
  /** Only a Computed identity counts toward the savings. */
  outcome?: SimulationOutcome
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
    coordinatedCount: number
  }
  headroom: { cpu: HeadroomBreakdown; memory: HeadroomBreakdown }
  attention: Record<AttentionState, AttentionGroup>
  policies: PolicyRollup[]
}

/** The non-Safe Risk states, each with its own attention list. */
export type AttentionState = Exclude<RiskState, 'safe'>

/** The worst identities of one Risk state; total counts every identity in it. */
export interface AttentionGroup {
  total: number
  rows: AttentionRow[]
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
