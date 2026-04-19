const API_BASE = ''

export async function api<T = unknown>(path: string, opts?: RequestInit): Promise<T> {
  const res = await fetch(API_BASE + path, opts)
  if (!res.ok) {
    const err = await res.json().catch(() => ({ error: res.statusText }))
    throw new Error(err.error || res.statusText)
  }
  return res.json()
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
  { label: '7d', window: '168h', step: '15m' },
  { label: '30d', window: '720h', step: '1h' },
]

export const defaultTimeRange = '168h'

export function getTimeRangeStep(window: string): string {
  const opt = timeRangeOptions.find((o) => o.window === window)
  return opt ? opt.step : '5m'
}

// --- Types ---

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
}

export interface PolicySpec {
  spec?: {
    rightSizing?: {
      resourcesConfigs?: {
        cpu?: ResourceConfig
        memory?: ResourceConfig
      }
    }
  }
  conditions?: Condition[]
}

export interface ResourceConfig {
  window?: string
  requests?: {
    percentile?: number
    headroom?: number
    minAllowed?: string
    maxAllowed?: string
  }
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
  items: WorkloadItem[]
  total: number
  pageSize: number
  namespaces?: string[]
  kinds?: string[]
  counts: { total: number; automated: number; manual: number }
}

export interface PolicyWorkloadsData {
  items: WorkloadItem[]
  total: number
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
  oomEvents?: OOMEvent[]
}

export interface RecommendationContainer {
  cpuRequest?: string
  memoryRequest?: string
}

export interface RecommendationsData {
  automated: boolean
  policyName?: string
  containers?: Record<string, RecommendationContainer>
  cpuRecommendations?: Record<string, TimeValue[]>
  memoryRecommendations?: Record<string, TimeValue[]>
}

export interface SimulateRequest {
  namespace: string
  ownerKind: string
  ownerName: string
  window: string
  step: string
  cpu: SimulateResourceConfig
  memory: SimulateResourceConfig
}

export interface SimulateResourceConfig {
  percentile: number
  headroom: number
  window: string
  minAllowed?: string
  maxAllowed?: string
}

export interface SimulationResult {
  containers: Record<string, RecommendationContainer>
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
