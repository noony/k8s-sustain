<script setup lang="ts">
import { ref, reactive, computed, onMounted, onUnmounted, nextTick, watch } from 'vue'
import {
  api,
  ApiError,
  type PolicySummary,
  type SimulationResult,
  type SimulateRequest,
  type SimulationOverride,
  type SimulationSettings,
  type RecommendationContainer,
  type Resource,
  type WorkloadListData,
} from '../lib/api'
import { parseCPUQuantity, parseMemoryQuantity, formatBytes, formatSeconds } from '../lib/format'
import { outcomeLabel } from '../lib/outcomes'
import { getKinds, type WorkloadKind } from '../lib/kinds'
import { limitModeOf, limitModeOrder, limitModes, limitsPatch, type LimitMode } from '../lib/limits'
import { patches, setPatch, valueAt, type KeyPath, type ValueAt } from '../lib/patch'
import type { Chart } from 'chart.js'
import {
  createTimeSeriesChart,
  destroyAllCharts,
  groupOOMKillsByContainer,
  zoomedRangeSeconds,
} from '../lib/chart'
import { chartResources, recommendedSeries, runningLines } from '../lib/resourceCharts'
import TimeRangePicker from '../components/TimeRangePicker.vue'
import { useTimeRange } from '../composables/useTimeRange'
import { simulateRangeBody, resolveRange, DEFAULT_RANGE, type TimeRange } from '../lib/timerange'
import DurationCombobox from '../components/DurationCombobox.vue'
import ResourceDiff from '../components/ResourceDiff.vue'
import RecommendationTrace from '../components/RecommendationTrace.vue'
import KpiCard from '../components/KpiCard.vue'
import ErrorState from '../components/ErrorState.vue'
import EmptyState from '../components/EmptyState.vue'
import PageHeader from '../components/PageHeader.vue'
import Combobox from '../components/Combobox.vue'

const props = defineProps<{
  namespace?: string
  kind?: string
  name?: string
}>()

const simNs = ref(props.namespace || '')
const simKind = ref(props.kind || '')
const simName = ref(props.name || '')
const { range } = useTimeRange()

const resources: { key: Resource; label: string; tag: string }[] = [
  { key: 'cpu', label: 'CPU', tag: 'sim-tag-cpu' },
  { key: 'memory', label: 'Memory', tag: 'sim-tag-mem' },
]

// Filled from the server's effective settings, never from defaults of its
// own: the server alone knows the Policy and the recommender's defaults.
interface ResourceForm {
  window: string
  percentile: number
  headroom: number
  minAllowed: string
  maxAllowed: string
  keepRequest: boolean
  limitMode: LimitMode
  limitRatio: number
}
function blankResource(): ResourceForm {
  return {
    window: '',
    percentile: 0,
    headroom: 0,
    minAllowed: '',
    maxAllowed: '',
    keepRequest: false,
    limitMode: 'keepLimit',
    limitRatio: 1,
  }
}
const form = reactive({
  cpu: blankResource(),
  memory: blankResource(),
  coordination: false,
  anchor: '',
  excludeInit: false,
})

// The user's edits, sent as a merge patch onto the starting Policy's
// settings; a field it does not name keeps the server's value.
const override = ref<SimulationOverride>({})
const basePolicy = ref('')

const policies = ref<PolicySummary[]>([])
const kinds = ref<WorkloadKind[]>([])
const kindsError = ref('')
const simData = ref<SimulationResult | null>(null)
const simError = ref('')
const overrideError = ref('')
const firstRun = ref(true)
const simulating = ref(false)
const liveMessage = ref('')
const workloadList = ref<WorkloadListData['items']>([])

type SettingPath = KeyPath<SimulationSettings>

const resourceSetting = (r: Resource) =>
  ({
    window: ['resourcesConfigs', r, 'window'],
    requests: ['resourcesConfigs', r, 'requests'],
    keepRequest: ['resourcesConfigs', r, 'requests', 'keepRequest'],
    percentile: ['resourcesConfigs', r, 'requests', 'percentile'],
    headroom: ['resourcesConfigs', r, 'requests', 'headroom'],
    minAllowed: ['resourcesConfigs', r, 'requests', 'minAllowed'],
    maxAllowed: ['resourcesConfigs', r, 'requests', 'maxAllowed'],
    limits: ['resourcesConfigs', r, 'limits'],
  }) as const satisfies Record<string, SettingPath>

const workloadSetting = {
  coordination: ['autoscalerCoordination', 'enabled'],
  anchor: ['autoscalerCoordination', 'replicaBudgetAnchor'],
  excludeInit: ['excludeInitContainers'],
} as const satisfies Record<string, SettingPath>

function edit<P extends SettingPath>(path: P, value: ValueAt<SimulationOverride, P>) {
  setPatch<SimulationSettings, P>(override.value, path, value)
  scheduleSimulation()
}

function setWindow(r: Resource, v: string) {
  form[r].window = v
  edit(resourceSetting(r).window, v || null)
}
function setRequest(r: Resource, key: 'percentile' | 'headroom', v: number) {
  form[r][key] = v
  edit(resourceSetting(r)[key], v)
}
function setBound(r: Resource, key: 'minAllowed' | 'maxAllowed', v: string) {
  form[r][key] = v
  edit(resourceSetting(r)[key], v || null)
}
// A kept request takes no percentile, headroom or bounds; letting it go
// again hands them back to the Policy.
function setKeepRequest(r: Resource, keep: boolean) {
  form[r].keepRequest = keep
  edit(
    resourceSetting(r).requests,
    keep
      ? { keepRequest: true, percentile: null, headroom: null, minAllowed: null, maxAllowed: null }
      : { keepRequest: false },
  )
}
function setLimit(r: Resource, mode: LimitMode, ratio: number) {
  form[r].limitMode = mode
  form[r].limitRatio = ratio
  edit(resourceSetting(r).limits, limitsPatch(mode, ratio))
}
function setCoordination(enabled: boolean) {
  form.coordination = enabled
  edit(workloadSetting.coordination, enabled)
}
function setAnchor(v: string) {
  form.anchor = v
  edit(workloadSetting.anchor, v === '' ? null : Number(v))
}
function setExcludeInit(v: boolean) {
  form.excludeInit = v
  edit(workloadSetting.excludeInit, v)
}

// Never over a control the user edited: the response may predate the edit.
function fill(eff: SimulationSettings) {
  const take = <P extends SettingPath>(
    p: P,
    set: (v: ValueAt<SimulationSettings, P> | undefined) => void,
  ) => {
    if (!patches<SimulationSettings, P>(override.value, p)) set(valueAt(eff, p))
  }
  for (const { key: r } of resources) {
    const f = form[r]
    const s = resourceSetting(r)
    take(s.window, (v) => (f.window = v ?? ''))
    take(s.keepRequest, (v) => (f.keepRequest = !!v))
    take(s.percentile, (v) => (f.percentile = v ?? f.percentile))
    take(s.headroom, (v) => (f.headroom = v ?? f.headroom))
    take(s.minAllowed, (v) => (f.minAllowed = v ?? ''))
    take(s.maxAllowed, (v) => (f.maxAllowed = v ?? ''))
    take(s.limits, (v) => {
      f.limitMode = limitModeOf(v)
      f.limitRatio = v?.requestsLimitsRatio ?? f.limitRatio
    })
  }
  take(workloadSetting.coordination, (v) => (form.coordination = !!v))
  take(workloadSetting.anchor, (v) => (form.anchor = v == null ? '' : String(v)))
  take(workloadSetting.excludeInit, (v) => (form.excludeInit = !!v))
}

const startingPoint = computed(() => {
  if (!simData.value) return ''
  if (simData.value.policy) return `Starting from policy ${simData.value.policy}`
  return 'No policy governs this workload: starting from the recommender defaults'
})

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

// Must stay within the server's pageSize cap (200) or the API answers 400.
const WORKLOAD_PAGE_SIZE = 200

async function loadWorkloads() {
  const items: WorkloadListData['items'] = []
  try {
    for (let page = 1; ; page++) {
      const list = await api<WorkloadListData>(
        `/api/workloads?pageSize=${WORKLOAD_PAGE_SIZE}&page=${page}`,
      )
      items.push(...(list.items || []))
      if (!list.items?.length || items.length >= list.total) break
    }
  } catch {
    // Dropdown options are best-effort; keep whatever pages loaded.
  }
  workloadList.value = items
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

async function loadKinds() {
  try {
    kinds.value = await getKinds()
    kindsError.value = ''
  } catch (e: any) {
    kindsError.value = e.message
    return
  }
  if (!simKind.value) simKind.value = kinds.value[0]?.name ?? ''
}

function startFrom(policy: string) {
  basePolicy.value = policy
  resetOverride()
}

function resetOverride() {
  override.value = {}
  scheduleSimulation()
}

function scheduleSimulation() {
  if (debounceTimer) clearTimeout(debounceTimer)
  debounceTimer = setTimeout(runSimulation, 400)
}

async function runSimulation() {
  debounceTimer = null
  if (!simNs.value || !simName.value) {
    requestId++
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
    override: override.value,
  }
  if (basePolicy.value) body.policy = basePolicy.value

  try {
    const data = await api<SimulationResult>('/api/simulate', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(body),
    })

    if (id !== requestId) return

    simData.value = data
    simError.value = ''
    overrideError.value = ''
    firstRun.value = false
    simulating.value = false
    liveMessage.value = `Simulation updated for ${simNs.value}/${simName.value}`
    fill(data.effective)

    await nextTick()
    renderCharts(data)
  } catch (e: any) {
    if (id !== requestId) return
    simulating.value = false
    liveMessage.value = `Simulation failed: ${e.message}`
    if (e instanceof ApiError && e.status === 400 && simData.value) {
      overrideError.value = e.message
      return
    }
    simError.value = e.message
    destroyAllCharts()
  }
}

function historyNotes(data: SimulationResult): string[] {
  const notes = ['Recommended as of each instant, the OOM floor as it stood then included.']
  if (data.effective.autoscalerCoordination.enabled) {
    notes.push("Coordinated with today's autoscaler at every instant: it has no history.")
  }
  return notes
}

function renderCharts(data: SimulationResult) {
  destroyAllCharts()
  const history = data.history
  if (!history) return

  const simResources = data.resources || {}
  const kills = groupOOMKillsByContainer(history.oomKills)
  const stepMs = history.stepSeconds * 1000
  const chartWindow = resolveRange(range.value, Date.now())
  const notes = historyNotes(data)

  for (const cname of Object.keys(data.containers || {})) {
    const containerHistory = history.containers[cname]
    if (!containerHistory) continue
    for (const r of chartResources) {
      const series = containerHistory[r.key]
      const { extra, annotations } = runningLines(
        r,
        series.runningRequest,
        series.runningLimit,
        simResources[cname] || {},
      )
      createTimeSeriesChart('sim' + r.canvas + '-' + cname, series.usage ?? [], {
        label: 'Usage percentile (busiest replica)',
        color: r.color,
        unit: r.unit,
        transform: r.transform,
        yFormat: r.yFormat,
        annotations,
        extraSeries: [
          ...extra,
          ...recommendedSeries(series.recommendedRequest, series.recommendedLimit),
        ],
        oomKills: r.marksOOMKills ? kills[cname] || [] : undefined,
        notes,
        onZoomComplete: onChartZoom,
        stepMs,
        window: chartWindow,
      })
    }
  }
}

// Counts only containers with both a current and a recommended request: a
// kept request is recommended nothing, and changes nothing.
function requestDelta(key: 'cpuRequest' | 'memoryRequest', parse: (q: string) => number): number {
  let delta = 0
  for (const [cname, rec] of Object.entries(simData.value?.containers ?? {})) {
    const current = simData.value?.resources?.[cname]?.[key]
    const recommended = rec[key]
    if (current && recommended) delta += parse(recommended) - parse(current)
  }
  return delta
}

function deltaTone(delta: number): 'positive' | 'danger' | 'neutral' {
  if (delta < 0) return 'positive'
  if (delta > 0) return 'danger'
  return 'neutral'
}

function cpuDeltaSummary(): string {
  if (!simData.value) return '-'
  const cores = requestDelta('cpuRequest', parseCPUQuantity)
  return `${cores > 0 ? '+' : ''}${cores.toFixed(2)} cores`
}

function cpuDeltaTone(): 'positive' | 'danger' | 'neutral' {
  return deltaTone(requestDelta('cpuRequest', parseCPUQuantity))
}

function memDeltaSummary(): string {
  if (!simData.value) return '-'
  const bytes = requestDelta('memoryRequest', parseMemoryQuantity)
  const sign = bytes > 0 ? '+' : bytes < 0 ? '-' : ''
  return `${sign}${formatBytes(Math.abs(bytes))}`
}

function memDeltaTone(): 'positive' | 'danger' | 'neutral' {
  return deltaTone(requestDelta('memoryRequest', parseMemoryQuantity))
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

watch([simNs, simKind, simName, range], scheduleSimulation)

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
  await Promise.all([loadPolicies(), loadWorkloads(), loadKinds()])
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

  <div class="card">
    <div class="card-header"><h2>Workload Target</h2></div>
    <ErrorState
      v-if="kindsError"
      title="Could not load the workload kinds"
      :message="kindsError"
      @retry="loadKinds"
    />
    <div v-else class="workload-selector">
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
          <option v-for="k in kinds" :key="k.name" :value="k.name">{{ k.name }}</option>
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

  <div v-if="simData" class="card">
    <div class="card-header">
      <h2>Configuration</h2>
      <div class="row">
        <span v-if="simulating" class="simulating-pill" aria-hidden="true">Simulating…</span>
        <div class="sr-only" aria-live="polite" aria-atomic="true">{{ liveMessage }}</div>
        <div class="sim-policy-loader">
          <label for="sim-policy">Start from</label>
          <select
            id="sim-policy"
            :value="basePolicy"
            @change="startFrom(($event.target as HTMLSelectElement).value)"
          >
            <option value="">Governing policy</option>
            <option v-for="p in policies" :key="p.name" :value="p.name">{{ p.name }}</option>
          </select>
        </div>
        <button
          class="btn btn-secondary btn-sm"
          type="button"
          data-test="reset-override"
          @click="resetOverride"
        >
          Reset
        </button>
      </div>
    </div>
    <p class="form-helper sim-starting-point" data-test="starting-point">{{ startingPoint }}</p>
    <p v-if="overrideError" class="form-error" role="alert" data-test="override-error">
      {{ overrideError }}
    </p>

    <div class="sim-grid">
      <div v-for="r in resources" :key="r.key" class="sim-section">
        <h3>
          <span class="sim-section-tag" :class="r.tag">{{ r.label }}</span> Configuration
        </h3>
        <div class="sim-subsection">
          <div class="sim-subsection-title">Requests</div>
          <div class="form-group">
            <label>Window</label>
            <DurationCombobox
              :model-value="form[r.key].window"
              @update:model-value="(v: string) => setWindow(r.key, v)"
            />
          </div>
          <label class="sim-check">
            <input
              type="checkbox"
              :checked="form[r.key].keepRequest"
              :data-test="r.key + '-keep-request'"
              @change="setKeepRequest(r.key, ($event.target as HTMLInputElement).checked)"
            />
            Keep the current request
          </label>
          <template v-if="!form[r.key].keepRequest">
            <div class="form-group">
              <label>Percentile</label>
              <div class="slider-row">
                <input
                  type="range"
                  :value="form[r.key].percentile"
                  min="1"
                  max="100"
                  :aria-label="r.label + ' percentile'"
                  :aria-valuetext="form[r.key].percentile + ' percent'"
                  :data-test="r.key + '-percentile'"
                  @input="
                    setRequest(
                      r.key,
                      'percentile',
                      Number(($event.target as HTMLInputElement).value),
                    )
                  "
                />
                <span class="slider-value">{{ form[r.key].percentile }}%</span>
              </div>
            </div>
            <div class="form-group">
              <label>Headroom</label>
              <div class="slider-row">
                <input
                  type="range"
                  :value="form[r.key].headroom"
                  min="0"
                  max="100"
                  :aria-label="r.label + ' headroom'"
                  :aria-valuetext="form[r.key].headroom + ' percent'"
                  @input="
                    setRequest(r.key, 'headroom', Number(($event.target as HTMLInputElement).value))
                  "
                />
                <span class="slider-value">{{ form[r.key].headroom }}%</span>
              </div>
            </div>
            <div class="form-group">
              <label>Min Allowed</label>
              <input
                type="text"
                :value="form[r.key].minAllowed"
                :placeholder="r.key === 'cpu' ? 'e.g. 50m' : 'e.g. 64Mi'"
                @input="setBound(r.key, 'minAllowed', ($event.target as HTMLInputElement).value)"
              />
            </div>
            <div class="form-group">
              <label>Max Allowed</label>
              <input
                type="text"
                :value="form[r.key].maxAllowed"
                :placeholder="r.key === 'cpu' ? 'e.g. 4000m' : 'e.g. 8Gi'"
                @input="setBound(r.key, 'maxAllowed', ($event.target as HTMLInputElement).value)"
              />
            </div>
          </template>
        </div>
        <div class="sim-subsection">
          <div class="sim-subsection-title">Limits</div>
          <div class="form-group">
            <label :for="r.key + '-limit-mode'">Mode</label>
            <select
              :id="r.key + '-limit-mode'"
              :value="form[r.key].limitMode"
              :aria-describedby="r.key + '-limit-help'"
              @change="
                setLimit(
                  r.key,
                  ($event.target as HTMLSelectElement).value as LimitMode,
                  form[r.key].limitRatio,
                )
              "
            >
              <option v-for="m in limitModeOrder" :key="m" :value="m">
                {{ limitModes[m].label }}
              </option>
            </select>
            <p :id="r.key + '-limit-help'" class="form-helper">
              {{ limitModes[form[r.key].limitMode].help[r.key] }}
            </p>
          </div>
          <div v-if="form[r.key].limitMode === 'requestsLimitsRatio'" class="form-group">
            <label>Ratio (limit ÷ request)</label>
            <input
              type="number"
              inputmode="decimal"
              placeholder="e.g. 1.5"
              :value="form[r.key].limitRatio"
              @input="
                setLimit(
                  r.key,
                  'requestsLimitsRatio',
                  Number(($event.target as HTMLInputElement).value),
                )
              "
            />
          </div>
        </div>
      </div>
    </div>

    <div class="sim-section sim-shared">
      <h3>Workload</h3>
      <label class="sim-check">
        <input
          type="checkbox"
          :checked="form.coordination"
          data-test="coordination"
          @change="setCoordination(($event.target as HTMLInputElement).checked)"
        />
        Coordinate with the autoscaler
      </label>
      <div v-if="form.coordination" class="form-group">
        <label>Replica budget anchor (0–1)</label>
        <input
          type="number"
          inputmode="decimal"
          step="0.05"
          placeholder="no replica correction"
          :value="form.anchor"
          @input="setAnchor(($event.target as HTMLInputElement).value)"
        />
      </div>
      <label class="sim-check">
        <input
          type="checkbox"
          :checked="form.excludeInit"
          @change="setExcludeInit(($event.target as HTMLInputElement).checked)"
        />
        Exclude init containers
      </label>
    </div>
  </div>

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
        <p v-if="simData.outcome !== 'Computed'" class="form-helper" data-test="sim-outcome">
          {{ outcomeLabel(simData.outcome) }}: the controller would not apply this yet.
        </p>
        <div class="stats-row">
          <KpiCard label="CPU change" :value="cpuDeltaSummary()" :tone="cpuDeltaTone()" />
          <KpiCard label="Memory change" :value="memDeltaSummary()" :tone="memDeltaTone()" />
        </div>
      </div>
      <div class="card">
        <div class="card-header">
          <h2>Simulation Results</h2>
        </div>
        <p class="form-helper">
          Computed from Prometheus alone: an OOM kill the controller's watcher saw in the last
          {{ formatSeconds(simData.liveOOMWindowSeconds) }}, not yet in Prometheus, raises the
          controller's memory floor but not this one.
        </p>
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
            <RecommendationTrace v-if="simData.trace?.[cname]" :trace="simData.trace[cname]" />
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
      <p v-if="simData.historyError" class="form-error" role="alert" data-test="history-error">
        History unavailable: {{ simData.historyError }}
      </p>
      <div
        v-for="cname in simData.history ? simRegularContainerNames() : []"
        :key="cname"
        class="card"
      >
        <div class="card-header">
          <h2>
            Container: <code>{{ cname }}</code>
          </h2>
        </div>
        <div class="chart-grid">
          <div v-for="r in chartResources" :key="r.key">
            <div class="chart-head">
              <h3 style="font-size: 13px; color: var(--text-dim); margin: 0">
                {{ r.label }} Usage + Recommendation
              </h3>
              <button v-if="range.kind === 'absolute'" class="reset-zoom-btn" @click="resetRange">
                Reset zoom
              </button>
            </div>
            <div class="chart-wrapper">
              <div class="chart-container">
                <canvas :id="'sim' + r.canvas + '-' + cname"></canvas>
              </div>
            </div>
          </div>
        </div>
      </div>

      <template v-if="simData.history && simInitContainerNames().length > 0">
        <h2 style="margin-top: 16px">Init containers</h2>
        <div v-for="cname in simInitContainerNames()" :key="cname" class="card">
          <div class="card-header">
            <h2>
              Init container: <code>{{ cname }}</code>
            </h2>
            <span class="badge">init</span>
          </div>
          <div class="chart-grid">
            <div v-for="r in chartResources" :key="r.key">
              <div class="chart-head">
                <h3 style="font-size: 13px; color: var(--text-dim); margin: 0">
                  {{ r.label }} Usage + Recommendation
                </h3>
                <button v-if="range.kind === 'absolute'" class="reset-zoom-btn" @click="resetRange">
                  Reset zoom
                </button>
              </div>
              <div class="chart-wrapper">
                <div class="chart-container">
                  <canvas :id="'sim' + r.canvas + '-' + cname"></canvas>
                </div>
              </div>
            </div>
          </div>
        </div>
      </template>
    </template>
  </template>
</template>
