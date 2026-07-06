<script setup lang="ts">
import { ref, computed, onMounted, onUnmounted, nextTick, watch } from 'vue'
import {
  api,
  type PolicySummary,
  type PolicySpec,
  type SimulationResult,
  type SimulateRequest,
  type RecommendationContainer,
  type ResourceLimitsConfig,
} from '../lib/api'
import { parseCPUQuantity, parseMemoryQuantity, parseStepToMs, formatBytes } from '../lib/format'
import type { Chart } from 'chart.js'
import {
  createTimeSeriesChart,
  destroyAllCharts,
  zoomedRangeSeconds,
  type ExtraSeries,
  type ChartAnnotation,
} from '../lib/chart'
import TimeRangePicker from '../components/TimeRangePicker.vue'
import { useTimeRange } from '../composables/useTimeRange'
import { simulateRangeBody, DEFAULT_RANGE, type TimeRange } from '../lib/timerange'
import DurationCombobox from '../components/DurationCombobox.vue'
import ResourceDiff from '../components/ResourceDiff.vue'
import KpiCard from '../components/KpiCard.vue'
import ErrorState from '../components/ErrorState.vue'
import EmptyState from '../components/EmptyState.vue'
import PageHeader from '../components/PageHeader.vue'
import Combobox from '../components/Combobox.vue'
import { type WorkloadListData } from '../lib/api'

const props = defineProps<{
  namespace?: string
  kind?: string
  name?: string
}>()

// Form state
const simNs = ref(props.namespace || '')
const simKind = ref(props.kind || 'Deployment')
const simName = ref(props.name || '')
const { range } = useTimeRange()
const cpuWindow = ref('168h')
const memWindow = ref('168h')
const cpuPct = ref(95)
const cpuHr = ref(0)
const cpuMin = ref('')
const cpuMax = ref('')
const memPct = ref(95)
const memHr = ref(0)
const memMin = ref('')
const memMax = ref('')

// Limits config — at most one of these modes is active per resource.
// "Keep existing limit" is the default; it emits { keepLimit: true } so the
// payload is always explicit (an unset limits field would be semantically
// identical but introduces two synonyms in the UI).
type LimitMode =
  'keepLimit' | 'noLimit' | 'equalsToRequest' | 'requestsLimitsRatio' | 'keepLimitRequestRatio'
const cpuLimitMode = ref<LimitMode>('keepLimit')
const cpuLimitRatio = ref(1)
const memLimitMode = ref<LimitMode>('keepLimit')
const memLimitRatio = ref(1)

// State
const policies = ref<PolicySummary[]>([])
const selectedPolicy = ref('')
const simData = ref<SimulationResult | null>(null)
const simError = ref('')
const firstRun = ref(true)
const simulating = ref(false)
const liveMessage = ref('')
const workloadList = ref<WorkloadListData['items']>([])

function limitHelpText(mode: LimitMode, kind: 'cpu' | 'memory'): string {
  switch (mode) {
    case 'keepLimit':
      return 'Existing pod limits stay unchanged.'
    case 'noLimit':
      return kind === 'cpu'
        ? 'Strips the CPU limit from the pod.'
        : 'Strips the memory limit from the pod.'
    case 'equalsToRequest':
      return kind === 'cpu'
        ? 'Limit = request. Note: CPU is throttled at the request value.'
        : 'Limit = request. Memory above the request value triggers OOM-kill.'
    case 'requestsLimitsRatio':
      return 'Limit = request × ratio.'
    case 'keepLimitRequestRatio':
      return 'Limit = request × (current limit ÷ current request), per container.'
  }
}

const cpuLimitHelp = computed(() => limitHelpText(cpuLimitMode.value, 'cpu'))
const memLimitHelp = computed(() => limitHelpText(memLimitMode.value, 'memory'))

// Validation — mirrors the CRD constraints exactly so the user sees errors
// inline instead of waiting for the backend 400.
const WINDOW_RE = /^([0-9]+(m|h|d|w|y))+$/
const QUANTITY_RE = /^[+-]?(\d+(\.\d*)?|\.\d+)(e[+-]?\d+|[EPTGMK]i?|m|n|u)?$/

function validateWindow(s: string): string {
  if (!s) return ''
  return WINDOW_RE.test(s) ? '' : 'Must be a Prometheus duration (e.g. 168h, 7d, 1h30m)'
}
function validateQuantity(s: string): string {
  if (!s) return ''
  return QUANTITY_RE.test(s) ? '' : 'Invalid Kubernetes quantity (e.g. 100m, 64Mi, 2Gi)'
}
function validateMinMax(min: string, max: string, parser: (s: string) => number): string {
  if (!min || !max) return ''
  const lo = parser(min)
  const hi = parser(max)
  if (isNaN(lo) || isNaN(hi)) return ''
  return lo > hi ? 'Min must be ≤ Max' : ''
}
function validateRatio(r: number): string {
  return r >= 1 ? '' : 'Ratio must be ≥ 1'
}

const cpuWindowError = computed(() => validateWindow(cpuWindow.value))
const memWindowError = computed(() => validateWindow(memWindow.value))
const cpuMinError = computed(() => validateQuantity(cpuMin.value))
const cpuMaxError = computed(() => validateQuantity(cpuMax.value))
const cpuMinMaxError = computed(() =>
  cpuMinError.value || cpuMaxError.value
    ? ''
    : validateMinMax(cpuMin.value, cpuMax.value, parseCPUQuantity),
)
const memMinError = computed(() => validateQuantity(memMin.value))
const memMaxError = computed(() => validateQuantity(memMax.value))
const memMinMaxError = computed(() =>
  memMinError.value || memMaxError.value
    ? ''
    : validateMinMax(memMin.value, memMax.value, parseMemoryQuantity),
)
const cpuRatioError = computed(() =>
  cpuLimitMode.value === 'requestsLimitsRatio' ? validateRatio(cpuLimitRatio.value) : '',
)
const memRatioError = computed(() =>
  memLimitMode.value === 'requestsLimitsRatio' ? validateRatio(memLimitRatio.value) : '',
)
const hasValidationErrors = computed(
  () =>
    !!(
      cpuWindowError.value ||
      memWindowError.value ||
      cpuMinError.value ||
      cpuMaxError.value ||
      cpuMinMaxError.value ||
      memMinError.value ||
      memMaxError.value ||
      memMinMaxError.value ||
      cpuRatioError.value ||
      memRatioError.value
    ),
)

const namespaceOptions = computed(() =>
  Array.from(new Set(workloadList.value.map((w) => w.namespace))),
)
const nameOptions = computed(() =>
  Array.from(
    new Set(
      workloadList.value
        .filter((w) => (!simNs.value || w.namespace === simNs.value) && w.kind === simKind.value)
        .map((w) => w.name),
    ),
  ),
)

async function loadWorkloads() {
  try {
    const list = await api<WorkloadListData>('/api/workloads?pageSize=500')
    workloadList.value = list.items || []
  } catch {
    workloadList.value = []
  }
}
let debounceTimer: ReturnType<typeof setTimeout> | null = null
let requestId = 0

function simInitContainerSet(): Set<string> {
  return new Set(simData.value?.initContainers || [])
}

function simRegularContainers(): [string, RecommendationContainer][] {
  if (!simData.value) return []
  const inits = simInitContainerSet()
  return Object.entries(simData.value.containers).filter(([name]) => !inits.has(name))
}

function simInitContainers(): [string, RecommendationContainer][] {
  if (!simData.value) return []
  const inits = simInitContainerSet()
  return Object.entries(simData.value.containers).filter(([name]) => inits.has(name))
}

function simAllContainers(): { name: string; rec: RecommendationContainer; isInit: boolean }[] {
  return [
    ...simRegularContainers().map(([name, rec]) => ({ name, rec, isInit: false })),
    ...simInitContainers().map(([name, rec]) => ({ name, rec, isInit: true })),
  ]
}

function simRegularContainerNames(): string[] {
  return simRegularContainers().map(([name]) => name)
}

function simInitContainerNames(): string[] {
  return simInitContainers().map(([name]) => name)
}

async function loadPolicies() {
  try {
    policies.value = await api<PolicySummary[]>('/api/policies')
  } catch {
    policies.value = []
  }
}

function limitModeFromCfg(lim: ResourceLimitsConfig | undefined): {
  mode: LimitMode
  ratio: number
} {
  if (!lim) return { mode: 'keepLimit', ratio: 1 }
  if (lim.keepLimit) return { mode: 'keepLimit', ratio: 1 }
  if (lim.noLimit) return { mode: 'noLimit', ratio: 1 }
  if (lim.equalsToRequest) return { mode: 'equalsToRequest', ratio: 1 }
  if (lim.keepLimitRequestRatio) return { mode: 'keepLimitRequestRatio', ratio: 1 }
  if (typeof lim.requestsLimitsRatio === 'number') {
    return { mode: 'requestsLimitsRatio', ratio: lim.requestsLimitsRatio }
  }
  return { mode: 'keepLimit', ratio: 1 }
}

async function loadPolicyConfig(policyName: string) {
  if (!policyName) return
  try {
    const p = await api<PolicySpec>(`/api/policies/${encodeURIComponent(policyName)}`)
    const rs = p.spec?.rightSizing?.resourcesConfigs || {}
    const cpuCfg = rs.cpu || {}
    const memCfg = rs.memory || {}
    const cpuReq = cpuCfg.requests || {}
    const memReq = memCfg.requests || {}

    cpuPct.value = cpuReq.percentile || 95
    cpuHr.value = cpuReq.headroom || 0
    cpuMin.value = cpuReq.minAllowed || ''
    cpuMax.value = cpuReq.maxAllowed || ''
    memPct.value = memReq.percentile || 95
    memHr.value = memReq.headroom || 0
    memMin.value = memReq.minAllowed || ''
    memMax.value = memReq.maxAllowed || ''
    if (cpuCfg.window) cpuWindow.value = cpuCfg.window
    if (memCfg.window) memWindow.value = memCfg.window
    const cpuLim = limitModeFromCfg(cpuCfg.limits)
    cpuLimitMode.value = cpuLim.mode
    cpuLimitRatio.value = cpuLim.ratio
    const memLim = limitModeFromCfg(memCfg.limits)
    memLimitMode.value = memLim.mode
    memLimitRatio.value = memLim.ratio

    scheduleSimulation()
  } catch (e) {
    console.error('Failed to load policy:', e)
  }
}

function buildLimitsPayload(mode: LimitMode, ratio: number) {
  switch (mode) {
    case 'keepLimit':
      return { keepLimit: true }
    case 'noLimit':
      return { noLimit: true }
    case 'equalsToRequest':
      return { equalsToRequest: true }
    case 'keepLimitRequestRatio':
      return { keepLimitRequestRatio: true }
    case 'requestsLimitsRatio':
      return { requestsLimitsRatio: ratio }
  }
}

function scheduleSimulation() {
  if (debounceTimer) clearTimeout(debounceTimer)
  if (hasValidationErrors.value) {
    simulating.value = false
    return
  }
  debounceTimer = setTimeout(runSimulation, 400)
}

async function runSimulation() {
  if (!simNs.value || !simName.value) {
    simData.value = null
    simulating.value = false
    destroyAllCharts()
    return
  }

  const id = ++requestId
  simulating.value = true
  liveMessage.value = ''

  const body: SimulateRequest = {
    namespace: simNs.value,
    ownerKind: simKind.value,
    ownerName: simName.value,
    ...simulateRangeBody(range.value, Date.now()),
    cpu: { percentile: cpuPct.value, headroom: cpuHr.value, window: cpuWindow.value },
    memory: { percentile: memPct.value, headroom: memHr.value, window: memWindow.value },
  }
  if (cpuMin.value) body.cpu.minAllowed = cpuMin.value
  if (cpuMax.value) body.cpu.maxAllowed = cpuMax.value
  if (memMin.value) body.memory.minAllowed = memMin.value
  if (memMax.value) body.memory.maxAllowed = memMax.value
  body.cpu.limits = buildLimitsPayload(cpuLimitMode.value, cpuLimitRatio.value)
  body.memory.limits = buildLimitsPayload(memLimitMode.value, memLimitRatio.value)

  try {
    const data = await api<SimulationResult>('/api/simulate', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(body),
    })

    if (id !== requestId) return

    simData.value = data
    simError.value = ''
    firstRun.value = false
    simulating.value = false
    liveMessage.value = `Simulation updated for ${simNs.value}/${simName.value}`

    await nextTick()
    renderCharts(data)
  } catch (e: any) {
    if (id === requestId) {
      simError.value = e.message
      simulating.value = false
      liveMessage.value = `Simulation failed: ${e.message}`
      destroyAllCharts()
    }
  }
}

function renderCharts(data: SimulationResult) {
  destroyAllCharts()

  const containers = Object.keys(data.containers || {})
  const simResources = data.resources || {}
  const simCpuReq = data.cpuRequests || {}
  const simMemReq = data.memoryRequests || {}
  const simCpuRec = data.cpuRecommendations || {}
  const simMemRec = data.memoryRecommendations || {}
  const stepMs = parseStepToMs(simulateRangeBody(range.value, Date.now()).step)

  containers.forEach((cname) => {
    const res = simResources[cname] || {}

    // CPU chart
    if (data.cpuSeries?.[cname]) {
      const cpuAnnotations: ChartAnnotation[] = []
      const cpuExtra: ExtraSeries[] = []
      if (simCpuReq[cname]?.length) {
        cpuExtra.push({ data: simCpuReq[cname], label: 'Request', color: '#f59e0b', dash: [4, 4] })
      } else if (res.cpuRequest) {
        cpuAnnotations.push({
          value: parseCPUQuantity(res.cpuRequest),
          label: 'Request: ' + res.cpuRequest,
          color: '#f59e0b',
          dash: [4, 4],
        })
      }
      if (res.cpuLimit)
        cpuAnnotations.push({
          value: parseCPUQuantity(res.cpuLimit),
          label: 'Limit: ' + res.cpuLimit,
          color: '#f97316',
          dash: [2, 2],
        })
      if (simCpuRec[cname]?.length) {
        cpuExtra.push({
          data: simCpuRec[cname],
          label: 'Recommendation',
          color: '#ef4444',
          dash: [8, 4],
          stepped: false,
        })
      }
      createTimeSeriesChart('simcpu-' + cname, data.cpuSeries[cname], {
        label: 'CPU Usage',
        color: 'rgb(124, 58, 237)',
        unit: 'cores',
        yFormat: (v) => v.toFixed(3),
        annotations: cpuAnnotations,
        extraSeries: cpuExtra,
        onZoomComplete: onChartZoom,
        stepMs,
      })
    }

    // Memory chart
    if (data.memorySeries?.[cname]) {
      const memAnnotations: ChartAnnotation[] = []
      const memExtra: ExtraSeries[] = []
      if (simMemReq[cname]?.length) {
        memExtra.push({ data: simMemReq[cname], label: 'Request', color: '#f59e0b', dash: [4, 4] })
      } else if (res.memoryRequest) {
        memAnnotations.push({
          value: parseMemoryQuantity(res.memoryRequest),
          label: 'Request: ' + res.memoryRequest,
          color: '#f59e0b',
          dash: [4, 4],
        })
      }
      if (res.memoryLimit)
        memAnnotations.push({
          value: parseMemoryQuantity(res.memoryLimit),
          label: 'Limit: ' + res.memoryLimit,
          color: '#f97316',
          dash: [2, 2],
        })
      if (simMemRec[cname]?.length) {
        memExtra.push({
          data: simMemRec[cname],
          label: 'Recommendation',
          color: '#ef4444',
          dash: [8, 4],
          stepped: false,
        })
      }
      createTimeSeriesChart('simmem-' + cname, data.memorySeries[cname], {
        label: 'Memory Usage',
        color: '#06b6d4',
        unit: 'MiB',
        transform: (v) => v / (1024 * 1024),
        yFormat: (v) => v.toFixed(0),
        annotations: memAnnotations,
        extraSeries: memExtra,
        onZoomComplete: onChartZoom,
        stepMs,
      })
    }
  })
}

function cpuDeltaSummary(): string {
  if (!simData.value) return '-'
  let curMillis = 0,
    recMillis = 0
  for (const [cname, rec] of Object.entries(simData.value.containers)) {
    const cur = simData.value.resources?.[cname]?.cpuRequest
    if (cur) curMillis += parseCPUQuantity(cur) * 1000
    if (rec.cpuRequest) recMillis += parseCPUQuantity(rec.cpuRequest) * 1000
  }
  const deltaMillis = recMillis - curMillis
  const sign = deltaMillis > 0 ? '+' : ''
  return `${sign}${(deltaMillis / 1000).toFixed(2)} cores`
}

function cpuDeltaTone(): 'positive' | 'danger' | 'neutral' {
  if (!simData.value) return 'neutral'
  let curMillis = 0,
    recMillis = 0
  for (const [cname, rec] of Object.entries(simData.value.containers)) {
    const cur = simData.value.resources?.[cname]?.cpuRequest
    if (cur) curMillis += parseCPUQuantity(cur) * 1000
    if (rec.cpuRequest) recMillis += parseCPUQuantity(rec.cpuRequest) * 1000
  }
  if (recMillis < curMillis) return 'positive'
  if (recMillis > curMillis) return 'danger'
  return 'neutral'
}

function memDeltaSummary(): string {
  if (!simData.value) return '-'
  let curBytes = 0,
    recBytes = 0
  for (const [cname, rec] of Object.entries(simData.value.containers)) {
    const cur = simData.value.resources?.[cname]?.memoryRequest
    if (cur) curBytes += parseMemoryQuantity(cur)
    if (rec.memoryRequest) recBytes += parseMemoryQuantity(rec.memoryRequest)
  }
  const delta = recBytes - curBytes
  const sign = delta > 0 ? '+' : delta < 0 ? '-' : ''
  return `${sign}${formatBytes(Math.abs(delta))}`
}

function memDeltaTone(): 'positive' | 'danger' | 'neutral' {
  if (!simData.value) return 'neutral'
  let curBytes = 0,
    recBytes = 0
  for (const [cname, rec] of Object.entries(simData.value.containers)) {
    const cur = simData.value.resources?.[cname]?.memoryRequest
    if (cur) curBytes += parseMemoryQuantity(cur)
    if (rec.memoryRequest) recBytes += parseMemoryQuantity(rec.memoryRequest)
  }
  if (recBytes < curBytes) return 'positive'
  if (recBytes > curBytes) return 'danger'
  return 'neutral'
}

async function trySample() {
  try {
    const list = await api<WorkloadListData>('/api/workloads?automated=true&pageSize=1')
    const item = list.items?.[0]
    if (!item) return
    simNs.value = item.namespace
    simKind.value = item.kind
    simName.value = item.name
  } catch (e) {
    console.error('Failed to load sample workload:', e)
  }
}

// Watch all form inputs for debounced auto-simulation
watch(
  [
    simNs,
    simKind,
    simName,
    range,
    cpuWindow,
    memWindow,
    cpuPct,
    cpuHr,
    cpuMin,
    cpuMax,
    cpuLimitMode,
    cpuLimitRatio,
    memPct,
    memHr,
    memMin,
    memMax,
    memLimitMode,
    memLimitRatio,
  ],
  scheduleSimulation,
)

// Drag-to-zoom sets the shared range to the selected window — updating the URL
// + picker and re-running the simulation at finer resolution. The previous
// (relative) range is kept so "Reset zoom" restores it.
const prevRange = ref<TimeRange | null>(null)
function onChartZoom(chart: Chart) {
  const z = zoomedRangeSeconds(chart)
  if (!z) return
  if (range.value.kind === 'relative') prevRange.value = range.value
  range.value = { kind: 'absolute', fromTs: z.fromTs, toTs: z.toTs }
}
function resetRange() {
  range.value = prevRange.value ?? DEFAULT_RANGE
  prevRange.value = null
}

onMounted(async () => {
  await Promise.all([loadPolicies(), loadWorkloads()])
  if (props.namespace && props.name) {
    runSimulation()
  }
})

onUnmounted(() => {
  if (debounceTimer) clearTimeout(debounceTimer)
  destroyAllCharts()
})
</script>

<template>
  <PageHeader
    title="Policy Simulator"
    subtitle="Simulate policy parameter changes and preview their effect on historical metrics"
  />

  <!-- Workload Target -->
  <div class="card">
    <div class="card-header"><h2>Workload Target</h2></div>
    <div class="workload-selector">
      <div class="form-group">
        <label for="sim-ns">Namespace</label>
        <Combobox
          v-model="simNs"
          :options="namespaceOptions"
          placeholder="default"
          all-label="(any namespace)"
          free-text
          :show-all="false"
          min-width="100%"
        />
      </div>
      <div class="form-group">
        <label for="sim-kind">Kind</label>
        <select id="sim-kind" v-model="simKind">
          <option value="Deployment">Deployment</option>
          <option value="StatefulSet">StatefulSet</option>
          <option value="DaemonSet">DaemonSet</option>
          <option value="CronJob">CronJob</option>
          <option value="Job">Job</option>
          <option value="Rollout">Rollout</option>
          <option value="Pod">Pod</option>
        </select>
      </div>
      <div class="form-group">
        <label for="sim-name">Name</label>
        <Combobox
          v-model="simName"
          :options="nameOptions"
          placeholder="my-app"
          free-text
          :show-all="false"
          min-width="100%"
        />
      </div>
    </div>
  </div>

  <!-- Configuration -->
  <div class="card">
    <div class="card-header">
      <h2>Configuration</h2>
      <div class="row">
        <span v-if="simulating" class="simulating-pill" aria-hidden="true">Simulating…</span>
        <div class="sr-only" aria-live="polite" aria-atomic="true">{{ liveMessage }}</div>
        <div class="sim-policy-loader">
          <label for="sim-policy">Load from policy</label>
          <select
            id="sim-policy"
            v-model="selectedPolicy"
            @change="loadPolicyConfig(selectedPolicy)"
          >
            <option value="">Select a policy</option>
            <option v-for="p in policies" :key="p.name" :value="p.name">{{ p.name }}</option>
          </select>
        </div>
      </div>
    </div>

    <div class="sim-grid">
      <!-- CPU -->
      <div class="sim-section">
        <h3><span class="sim-section-tag sim-tag-cpu">CPU</span> Configuration</h3>
        <div class="sim-subsection">
          <div class="sim-subsection-title">Requests</div>
          <div class="form-group" :class="{ 'has-error': cpuWindowError }">
            <label>Window</label>
            <DurationCombobox v-model="cpuWindow" />
            <p v-if="cpuWindowError" class="form-error" role="alert">{{ cpuWindowError }}</p>
          </div>
          <div class="form-group">
            <label>Percentile</label>
            <div class="slider-row">
              <input
                type="range"
                v-model.number="cpuPct"
                min="50"
                max="100"
                aria-label="CPU percentile"
                :aria-valuetext="cpuPct + ' percent'"
              />
              <span class="slider-value">{{ cpuPct }}%</span>
            </div>
          </div>
          <div class="form-group">
            <label>Headroom</label>
            <div class="slider-row">
              <input
                type="range"
                v-model.number="cpuHr"
                min="0"
                max="100"
                aria-label="CPU headroom"
                :aria-valuetext="cpuHr + ' percent'"
              />
              <span class="slider-value">{{ cpuHr }}%</span>
            </div>
          </div>
          <div class="form-group" :class="{ 'has-error': cpuMinError }">
            <label>Min Allowed</label>
            <input
              v-model="cpuMin"
              type="text"
              placeholder="e.g. 50m"
              :aria-invalid="!!cpuMinError"
            />
            <p v-if="cpuMinError" class="form-error" role="alert">{{ cpuMinError }}</p>
          </div>
          <div class="form-group" :class="{ 'has-error': cpuMaxError || cpuMinMaxError }">
            <label>Max Allowed</label>
            <input
              v-model="cpuMax"
              type="text"
              placeholder="e.g. 4000m"
              :aria-invalid="!!(cpuMaxError || cpuMinMaxError)"
            />
            <p v-if="cpuMaxError" class="form-error" role="alert">{{ cpuMaxError }}</p>
            <p v-else-if="cpuMinMaxError" class="form-error" role="alert">{{ cpuMinMaxError }}</p>
          </div>
        </div>
        <div class="sim-subsection">
          <div class="sim-subsection-title">Limits</div>
          <div class="form-group">
            <label for="cpu-limit-mode">Mode</label>
            <select id="cpu-limit-mode" v-model="cpuLimitMode" aria-describedby="cpu-limit-help">
              <option value="keepLimit">Keep existing limit</option>
              <option value="noLimit">Remove limit</option>
              <option value="equalsToRequest">Equal to request</option>
              <option value="requestsLimitsRatio">Multiple of request</option>
              <option value="keepLimitRequestRatio">Preserve current limit/request ratio</option>
            </select>
            <p id="cpu-limit-help" class="form-helper">{{ cpuLimitHelp }}</p>
          </div>
          <div
            v-if="cpuLimitMode === 'requestsLimitsRatio'"
            class="form-group"
            :class="{ 'has-error': cpuRatioError }"
          >
            <label>Ratio (limit ÷ request)</label>
            <input
              v-model.number="cpuLimitRatio"
              type="number"
              inputmode="decimal"
              placeholder="e.g. 1.5"
              :aria-invalid="!!cpuRatioError"
            />
            <p v-if="cpuRatioError" class="form-error" role="alert">{{ cpuRatioError }}</p>
          </div>
        </div>
      </div>

      <!-- Memory -->
      <div class="sim-section">
        <h3><span class="sim-section-tag sim-tag-mem">Memory</span> Configuration</h3>
        <div class="sim-subsection">
          <div class="sim-subsection-title">Requests</div>
          <div class="form-group" :class="{ 'has-error': memWindowError }">
            <label>Window</label>
            <DurationCombobox v-model="memWindow" />
            <p v-if="memWindowError" class="form-error" role="alert">{{ memWindowError }}</p>
          </div>
          <div class="form-group">
            <label>Percentile</label>
            <div class="slider-row">
              <input
                type="range"
                v-model.number="memPct"
                min="50"
                max="100"
                aria-label="Memory percentile"
                :aria-valuetext="memPct + ' percent'"
              />
              <span class="slider-value">{{ memPct }}%</span>
            </div>
          </div>
          <div class="form-group">
            <label>Headroom</label>
            <div class="slider-row">
              <input
                type="range"
                v-model.number="memHr"
                min="0"
                max="100"
                aria-label="Memory headroom"
                :aria-valuetext="memHr + ' percent'"
              />
              <span class="slider-value">{{ memHr }}%</span>
            </div>
          </div>
          <div class="form-group" :class="{ 'has-error': memMinError }">
            <label>Min Allowed</label>
            <input
              v-model="memMin"
              type="text"
              placeholder="e.g. 64Mi"
              :aria-invalid="!!memMinError"
            />
            <p v-if="memMinError" class="form-error" role="alert">{{ memMinError }}</p>
          </div>
          <div class="form-group" :class="{ 'has-error': memMaxError || memMinMaxError }">
            <label>Max Allowed</label>
            <input
              v-model="memMax"
              type="text"
              placeholder="e.g. 8Gi"
              :aria-invalid="!!(memMaxError || memMinMaxError)"
            />
            <p v-if="memMaxError" class="form-error" role="alert">{{ memMaxError }}</p>
            <p v-else-if="memMinMaxError" class="form-error" role="alert">{{ memMinMaxError }}</p>
          </div>
        </div>
        <div class="sim-subsection">
          <div class="sim-subsection-title">Limits</div>
          <div class="form-group">
            <label for="mem-limit-mode">Mode</label>
            <select id="mem-limit-mode" v-model="memLimitMode" aria-describedby="mem-limit-help">
              <option value="keepLimit">Keep existing limit</option>
              <option value="noLimit">Remove limit</option>
              <option value="equalsToRequest">Equal to request</option>
              <option value="requestsLimitsRatio">Multiple of request</option>
              <option value="keepLimitRequestRatio">Preserve current limit/request ratio</option>
            </select>
            <p id="mem-limit-help" class="form-helper">{{ memLimitHelp }}</p>
          </div>
          <div
            v-if="memLimitMode === 'requestsLimitsRatio'"
            class="form-group"
            :class="{ 'has-error': memRatioError }"
          >
            <label>Ratio (limit ÷ request)</label>
            <input
              v-model.number="memLimitRatio"
              type="number"
              inputmode="decimal"
              placeholder="e.g. 1.5"
              :aria-invalid="!!memRatioError"
            />
            <p v-if="memRatioError" class="form-error" role="alert">{{ memRatioError }}</p>
          </div>
        </div>
      </div>
    </div>
  </div>

  <!-- Empty state: no workload selected -->
  <EmptyState
    v-if="!simNs || !simName"
    icon="search"
    title="Pick a workload"
    message="Enter a namespace and name above to simulate, or try a sample workload."
  >
    <template #actions>
      <button class="btn btn-secondary btn-sm" type="button" @click="trySample">
        Try sample workload
      </button>
    </template>
  </EmptyState>

  <!-- Results -->
  <div v-if="firstRun && !simData && !simError"></div>
  <ErrorState v-else-if="simError" :message="simError" @retry="runSimulation" />
  <template v-else-if="simData">
    <EmptyState
      v-if="Object.keys(simData.containers).length === 0"
      icon="search"
      title="No metrics found"
      message="No metrics data for this workload. Make sure the namespace, kind, and name are correct."
    />
    <template v-else>
      <div class="card">
        <div class="card-header"><h2>Savings impact</h2></div>
        <div class="stats-row">
          <KpiCard label="CPU change" :value="cpuDeltaSummary()" :tone="cpuDeltaTone()" />
          <KpiCard label="Memory change" :value="memDeltaSummary()" :tone="memDeltaTone()" />
        </div>
      </div>
      <div class="card">
        <div class="card-header">
          <h2>Simulation Results</h2>
        </div>
        <div v-if="simAllContainers().length > 0" class="rec-grid">
          <div
            v-for="{ name: cname, rec, isInit } in simAllContainers()"
            :key="cname"
            class="rec-card"
          >
            <h4>
              <span>{{ cname }}</span>
              <span v-if="isInit" class="badge">init</span>
            </h4>
            <div class="rec-row">
              <span class="label">CPU Request</span>
              <ResourceDiff
                :current="(simData.resources || {})[cname]?.cpuRequest"
                :recommended="rec.cpuRequest"
                resource-type="cpu"
              />
            </div>
            <div class="rec-row">
              <span class="label">CPU Limit</span>
              <span v-if="rec.cpuLimitRemoved" class="recommended">— removed —</span>
              <ResourceDiff
                v-else
                :current="(simData.resources || {})[cname]?.cpuLimit"
                :recommended="rec.cpuLimit"
                resource-type="cpu"
              />
            </div>
            <div class="rec-row">
              <span class="label">Memory Request</span>
              <ResourceDiff
                :current="(simData.resources || {})[cname]?.memoryRequest"
                :recommended="rec.memoryRequest"
                resource-type="memory"
              />
            </div>
            <div class="rec-row">
              <span class="label">Memory Limit</span>
              <span v-if="rec.memoryLimitRemoved" class="recommended">— removed —</span>
              <ResourceDiff
                v-else
                :current="(simData.resources || {})[cname]?.memoryLimit"
                :recommended="rec.memoryLimit"
                resource-type="memory"
              />
            </div>
          </div>
        </div>
      </div>

      <div class="charts-toolbar">
        <h2>Usage over time</h2>
        <div class="charts-toolbar-control" role="group" aria-labelledby="charts-time-range-label">
          <span id="charts-time-range-label" class="charts-toolbar-control-label">
            Time range
          </span>
          <TimeRangePicker v-model="range" />
        </div>
      </div>
      <div v-for="cname in simRegularContainerNames()" :key="cname" class="card">
        <div class="card-header">
          <h2>
            Container: <code>{{ cname }}</code>
          </h2>
        </div>
        <div class="chart-grid">
          <div>
            <div class="chart-head">
              <h3 style="font-size: 13px; color: var(--text-dim); margin: 0">
                CPU Usage + Recommendation
              </h3>
              <button v-if="range.kind === 'absolute'" class="reset-zoom-btn" @click="resetRange">
                Reset zoom
              </button>
            </div>
            <div class="chart-wrapper">
              <div class="chart-container"><canvas :id="'simcpu-' + cname"></canvas></div>
            </div>
          </div>
          <div>
            <div class="chart-head">
              <h3 style="font-size: 13px; color: var(--text-dim); margin: 0">
                Memory Usage + Recommendation
              </h3>
              <button v-if="range.kind === 'absolute'" class="reset-zoom-btn" @click="resetRange">
                Reset zoom
              </button>
            </div>
            <div class="chart-wrapper">
              <div class="chart-container"><canvas :id="'simmem-' + cname"></canvas></div>
            </div>
          </div>
        </div>
      </div>

      <template v-if="simInitContainerNames().length > 0">
        <h2 style="margin-top: 16px">Init containers</h2>
        <div v-for="cname in simInitContainerNames()" :key="cname" class="card">
          <div class="card-header">
            <h2>
              Init container: <code>{{ cname }}</code>
            </h2>
            <span class="badge">init</span>
          </div>
          <div class="chart-grid">
            <div>
              <div class="chart-head">
                <h3 style="font-size: 13px; color: var(--text-dim); margin: 0">
                  CPU Usage + Recommendation
                </h3>
                <button v-if="range.kind === 'absolute'" class="reset-zoom-btn" @click="resetRange">
                  Reset zoom
                </button>
              </div>
              <div class="chart-wrapper">
                <div class="chart-container"><canvas :id="'simcpu-' + cname"></canvas></div>
              </div>
            </div>
            <div>
              <div class="chart-head">
                <h3 style="font-size: 13px; color: var(--text-dim); margin: 0">
                  Memory Usage + Recommendation
                </h3>
                <button v-if="range.kind === 'absolute'" class="reset-zoom-btn" @click="resetRange">
                  Reset zoom
                </button>
              </div>
              <div class="chart-wrapper">
                <div class="chart-container"><canvas :id="'simmem-' + cname"></canvas></div>
              </div>
            </div>
          </div>
        </div>
      </template>
    </template>
  </template>
</template>
