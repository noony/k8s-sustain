<script setup lang="ts">
import { ref, computed, onMounted, onUnmounted, watch, nextTick } from 'vue'
import {
  api,
  type MetricsData,
  type RecommendationContainer,
  type RecommendationOutcome,
  type WorkloadDetailSnapshot,
  type CoordinationFactors,
} from '../lib/api'
import { parseCPUQuantity, parseMemoryQuantity, parseStepToMs, timeAgo } from '../lib/format'
import type { Chart } from 'chart.js'
import {
  createTimeSeriesChart,
  destroyAllCharts,
  zoomedRangeSeconds,
  groupOOMEventsByContainer,
  type ExtraSeries,
  type ChartAnnotation,
} from '../lib/chart'
import { useAutoRefresh } from '../composables/useAutoRefresh'
import { useApi } from '../composables/useApi'
import { useTimeRange } from '../composables/useTimeRange'
import { rangeQueryParams, resolveRange, DEFAULT_RANGE, type TimeRange } from '../lib/timerange'
import TimeRangePicker from '../components/TimeRangePicker.vue'
import ResourceDiff from '../components/ResourceDiff.vue'
import RecommendationTrace from '../components/RecommendationTrace.vue'
import KpiCard from '../components/KpiCard.vue'
import RiskBadge from '../components/RiskBadge.vue'
import PageHeader from '../components/PageHeader.vue'
import LoadingState from '../components/LoadingState.vue'
import ErrorState from '../components/ErrorState.vue'
import EmptyState from '../components/EmptyState.vue'

const props = defineProps<{
  namespace: string
  kind: string
  name: string
}>()

const { range } = useTimeRange()
const loading = ref(true)
const error = ref('')
const metrics = ref<MetricsData | null>(null)

const snapshot = useApi<WorkloadDetailSnapshot>(() =>
  api<WorkloadDetailSnapshot>(`/api/workloads/${props.namespace}/${props.kind}/${props.name}`),
)

// The Recommendation the controller stored; the detail page never recomputes
// one (that is the Simulator's job).
const stored = computed(() => snapshot.data.value?.recommendation)

const outcomeLabels: Record<RecommendationOutcome, string> = {
  Computed: 'Computed',
  NoData: 'No data',
  TooYoung: 'Too young',
  FetchFailed: 'Fetch failed',
  Conflicted: 'Conflicted (frozen)',
}

async function load() {
  const p = new URLSearchParams(rangeQueryParams(range.value, Date.now()))
  try {
    const [m] = await Promise.all([
      api<MetricsData>(
        `/api/workloads/${props.namespace}/${props.kind}/${props.name}/metrics?${p.toString()}`,
      ),
      snapshot.run(),
    ])
    if (snapshot.error.value) throw new Error(snapshot.error.value)
    metrics.value = m
    error.value = ''
    loading.value = false
    await nextTick()
    renderCharts()
  } catch (e: any) {
    error.value = e.message
    loading.value = false
  }
}

useAutoRefresh(() => {
  if (range.value.kind === 'relative') {
    destroyAllCharts()
    load()
  }
})

onMounted(load)
onUnmounted(destroyAllCharts)

watch(range, () => {
  // Refetch in place. Don't flip `loading` — that unmounts the charts and
  // collapses the page, which scrolls the viewport to the top on every zoom.
  // renderCharts() swaps the canvases once the new data arrives.
  load()
})

// Drag-to-zoom on any chart sets the shared range to the selected window,
// which updates the URL + picker and refetches every chart at finer
// resolution. The previous (relative) range is kept so "Reset zoom" restores it.
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

function containers(): string[] {
  const s = new Set<string>()
  if (metrics.value?.cpu) Object.keys(metrics.value.cpu).forEach((k) => s.add(k))
  if (metrics.value?.memory) Object.keys(metrics.value.memory).forEach((k) => s.add(k))
  return Array.from(s)
}

function initContainerSet(): Set<string> {
  return new Set(metrics.value?.initContainers ?? [])
}

function regularContainers(): string[] {
  const inits = initContainerSet()
  return containers().filter((c) => !inits.has(c))
}

function initContainers(): string[] {
  const inits = initContainerSet()
  return containers().filter((c) => inits.has(c))
}

function regularRecContainers(): [string, RecommendationContainer][] {
  const inits = initContainerSet()
  const all = stored.value?.containers || {}
  return Object.entries(all).filter(([name]) => !inits.has(name))
}

function initRecContainers(): [string, RecommendationContainer][] {
  const inits = initContainerSet()
  const all = stored.value?.containers || {}
  return Object.entries(all).filter(([name]) => inits.has(name))
}

function allRecContainers(): { name: string; rec: RecommendationContainer; isInit: boolean }[] {
  return [
    ...regularRecContainers().map(([name, rec]) => ({ name, rec, isInit: false })),
    ...initRecContainers().map(([name, rec]) => ({ name, rec, isInit: true })),
  ]
}

function oomByContainer() {
  return groupOOMEventsByContainer(metrics.value?.oomEvents)
}

function renderCharts() {
  destroyAllCharts()
  if (!metrics.value) return

  const resources = metrics.value.resources || {}
  const cpuRequests = metrics.value.cpuRequests || {}
  const memoryRequests = metrics.value.memoryRequests || {}
  const cpuLimits = metrics.value.cpuLimits || {}
  const memoryLimits = metrics.value.memoryLimits || {}
  const storedRecs = stored.value?.containers || {}
  const ooms = oomByContainer()
  const stepMs = parseStepToMs(rangeQueryParams(range.value, Date.now()).step)
  const chartWindow = resolveRange(range.value, Date.now())

  containers().forEach((cname) => {
    const res = resources[cname] || {}

    if (metrics.value!.cpu?.[cname]) {
      const cpuAnnotations: ChartAnnotation[] = []
      const cpuExtra: ExtraSeries[] = []
      if (cpuRequests[cname]?.length) {
        cpuExtra.push({
          data: cpuRequests[cname],
          label: 'Request',
          color: 'config',
          dash: [4, 4],
        })
      } else if (res.cpuRequest) {
        cpuAnnotations.push({
          value: parseCPUQuantity(res.cpuRequest),
          label: 'Request: ' + res.cpuRequest,
          color: 'config',
          dash: [4, 4],
        })
      }
      if (cpuLimits[cname]?.length) {
        cpuExtra.push({
          data: cpuLimits[cname],
          label: 'Limit',
          color: 'config',
          dash: [1, 4],
        })
      } else if (res.cpuLimit) {
        cpuAnnotations.push({
          value: parseCPUQuantity(res.cpuLimit),
          label: 'Limit: ' + res.cpuLimit,
          color: 'config',
          dash: [1, 4],
        })
      }
      const cpuRec = storedRecs[cname]?.cpuRequest
      if (cpuRec) {
        cpuAnnotations.push({
          value: parseCPUQuantity(cpuRec),
          label: 'Recommendation: ' + cpuRec,
          color: 'rec',
          dash: [8, 4],
        })
      }
      createTimeSeriesChart('cpu-' + cname, metrics.value!.cpu[cname], {
        label: 'CPU Usage',
        color: 'cpu',
        unit: 'cores',
        yFormat: (v) => v.toFixed(3),
        annotations: cpuAnnotations,
        extraSeries: cpuExtra,
        onZoomComplete: onChartZoom,
        stepMs,
        window: chartWindow,
      })
    }

    if (metrics.value!.memory?.[cname]) {
      const memAnnotations: ChartAnnotation[] = []
      const memExtra: ExtraSeries[] = []
      if (memoryRequests[cname]?.length) {
        memExtra.push({
          data: memoryRequests[cname],
          label: 'Request',
          color: 'config',
          dash: [4, 4],
        })
      } else if (res.memoryRequest) {
        memAnnotations.push({
          value: parseMemoryQuantity(res.memoryRequest),
          label: 'Request: ' + res.memoryRequest,
          color: 'config',
          dash: [4, 4],
        })
      }
      if (memoryLimits[cname]?.length) {
        memExtra.push({
          data: memoryLimits[cname],
          label: 'Limit',
          color: 'config',
          dash: [1, 4],
        })
      } else if (res.memoryLimit) {
        memAnnotations.push({
          value: parseMemoryQuantity(res.memoryLimit),
          label: 'Limit: ' + res.memoryLimit,
          color: 'config',
          dash: [1, 4],
        })
      }
      const memRec = storedRecs[cname]?.memoryRequest
      if (memRec) {
        memAnnotations.push({
          value: parseMemoryQuantity(memRec),
          label: 'Recommendation: ' + memRec,
          color: 'rec',
          dash: [8, 4],
        })
      }
      createTimeSeriesChart('mem-' + cname, metrics.value!.memory[cname], {
        label: 'Memory Usage',
        color: 'mem',
        unit: 'MiB',
        transform: (v) => v / (1024 * 1024),
        yFormat: (v) => v.toFixed(0),
        annotations: memAnnotations,
        extraSeries: memExtra,
        oomEvents: ooms[cname] || [],
        onZoomComplete: onChartZoom,
        stepMs,
        window: chartWindow,
      })
    }
  })
}

function driftLabel(): string {
  const s = snapshot.data.value
  if (!s) return '-'
  if (s.totalPods) return `${s.stalePods}/${s.totalPods} pods`
  return s.stalePods > 0 ? String(s.stalePods) : '-'
}

function isMeaningful(v: number | undefined): v is number {
  return typeof v === 'number' && Math.abs(v - 1) > 1e-6
}

function hasCoordinationFactors(cf?: CoordinationFactors): boolean {
  if (!cf?.enabled) return false
  return (
    isMeaningful(cf.cpuOverhead) || isMeaningful(cf.memoryOverhead) || isMeaningful(cf.cpuReplica)
  )
}
</script>

<template>
  <LoadingState v-if="loading" variant="kpi" message="Loading workload…" />
  <ErrorState v-else-if="error" :message="error" @retry="load" />
  <template v-else-if="metrics && snapshot.data.value">
    <div class="breadcrumb">
      <RouterLink to="/workloads">Workloads</RouterLink><span>/</span><span>{{ name }}</span>
    </div>

    <PageHeader :title="name">
      <template #title>
        <span class="kind-badge" :class="'kind-' + kind">{{ kind }}</span>
        {{ name }}
      </template>
      <template #subtitle>
        <span class="meta-chips">
          <span class="meta-chip"><span class="meta-key">Namespace</span>{{ namespace }}</span>
          <span class="meta-chip"
            ><span class="meta-key">Containers</span>{{ containers().length }}</span
          >
          <template v-if="snapshot.data.value.automated">
            <span class="badge badge-green">Automated</span>
            <RouterLink class="meta-chip" :to="`/policies/${snapshot.data.value.policyName}`"
              ><span class="meta-key">Policy</span>{{ snapshot.data.value.policyName }}</RouterLink
            >
          </template>
          <span
            v-else-if="snapshot.data.value.conflictingPolicies?.length"
            class="badge badge-red"
            :title="'Members opt into ' + snapshot.data.value.conflictingPolicies.join(', ')"
            >Conflicted: {{ snapshot.data.value.conflictingPolicies.join(' / ') }}</span
          >
          <span v-else class="badge badge-dim">Manual</span>
          <span v-if="snapshot.data.value.departed" class="badge badge-dim">Departed</span>
        </span>
      </template>
      <template #meta>
        <RiskBadge v-if="snapshot.data.value" :state="snapshot.data.value.riskState" />
        <span v-if="snapshot.data.value?.coordinationFactors?.enabled" class="badge badge-blue"
          >Coordinated<template
            v-if="hasCoordinationFactors(snapshot.data.value.coordinationFactors)"
          >
            <span v-if="isMeaningful(snapshot.data.value.coordinationFactors.cpuOverhead)">
              &times;{{ snapshot.data.value.coordinationFactors.cpuOverhead!.toFixed(2) }} CPU</span
            ><span v-if="isMeaningful(snapshot.data.value.coordinationFactors.memoryOverhead)">
              &times;{{
                snapshot.data.value.coordinationFactors.memoryOverhead!.toFixed(2)
              }}
              mem</span
            ><span v-if="isMeaningful(snapshot.data.value.coordinationFactors.cpuReplica)">
              &middot; replica &times;{{
                snapshot.data.value.coordinationFactors.cpuReplica!.toFixed(2)
              }}</span
            >
          </template></span
        >
      </template>
      <template #actions>
        <TimeRangePicker v-model="range" />
      </template>
    </PageHeader>

    <!-- Status snapshot -->
    <div class="card">
      <div class="card-header"><h2>Status</h2></div>
      <div class="stats-row">
        <KpiCard label="Mode" :value="snapshot.data.value?.updateMode || '-'" />
        <KpiCard
          label="Drift"
          :value="driftLabel()"
          :tone="snapshot.data.value && snapshot.data.value.stalePods > 0 ? 'warn' : 'neutral'"
        />
        <KpiCard
          label="OOM 24h"
          :value="String(snapshot.data.value?.oom24h || 0)"
          :tone="snapshot.data.value && snapshot.data.value.oom24h > 0 ? 'danger' : 'neutral'"
        />
      </div>
    </div>

    <!-- Blocked band -->
    <div v-if="snapshot.data.value?.blocked" class="card card-danger">
      <div class="card-header"><h2 class="text-error">Currently blocked</h2></div>
      <p>
        Reason: <code>{{ snapshot.data.value.blocked.reason }}</code> ·
        {{ snapshot.data.value.blocked.attempts }} attempts
      </p>
      <p v-if="snapshot.data.value.blocked.lastError" class="text-dim">
        Last error: {{ snapshot.data.value.blocked.lastError }}
      </p>
    </div>

    <!-- Stored Recommendation -->
    <div v-if="stored" class="card">
      <div class="card-header">
        <h2>Recommendation</h2>
        <span v-if="stored.outcome" class="badge" data-test="outcome">{{
          outcomeLabels[stored.outcome] ?? stored.outcome
        }}</span>
        <span v-if="stored.observedAt" class="text-dim" :title="stored.observedAt"
          >computed {{ timeAgo(stored.observedAt) }}</span
        >
      </div>
      <p v-if="allRecContainers().length === 0" class="text-dim">No Recommendation stored yet.</p>
      <div v-else class="container-grid">
        <div
          v-for="{ name: cname, rec, isInit } in allRecContainers()"
          :key="cname"
          class="container-card"
        >
          <h4>
            <span>{{ cname }}</span>
            <span v-if="isInit" class="badge">init</span>
          </h4>
          <div class="resource-row">
            <span class="label">CPU Request</span>
            <ResourceDiff
              :current="(metrics.resources || {})[cname]?.cpuRequest"
              :recommended="rec.cpuRequest"
              resource-type="cpu"
            />
          </div>
          <div class="resource-row">
            <span class="label">Memory Request</span>
            <ResourceDiff
              :current="(metrics.resources || {})[cname]?.memoryRequest"
              :recommended="rec.memoryRequest"
              resource-type="memory"
            />
          </div>
          <RecommendationTrace v-if="stored.trace?.[cname]" :trace="stored.trace[cname]" />
        </div>
      </div>
    </div>

    <!-- Charts per container -->
    <template v-if="regularContainers().length > 0">
      <h2 v-if="initContainers().length > 0" class="mt-3">Containers</h2>
    </template>
    <div v-for="cname in regularContainers()" :key="cname" class="card">
      <div class="card-header">
        <h2>
          Container: <code>{{ cname }}</code>
        </h2>
      </div>
      <div class="chart-grid">
        <div>
          <div class="chart-head">
            <div class="section-label">CPU Usage (cores)</div>
            <button v-if="range.kind === 'absolute'" class="reset-zoom-btn" @click="resetRange">
              Reset zoom
            </button>
          </div>
          <div class="chart-wrapper">
            <div class="chart-container"><canvas :id="'cpu-' + cname"></canvas></div>
          </div>
        </div>
        <div>
          <div class="chart-head">
            <div class="section-label" style="display: inline-flex; align-items: center">
              Memory Usage (MiB)
              <span v-if="(oomByContainer()[cname] || []).length > 0" class="oom-legend">
                <span class="oom-marker"></span>
                {{ oomByContainer()[cname].length }} OOM kill{{
                  oomByContainer()[cname].length > 1 ? 's' : ''
                }}
              </span>
            </div>
            <button v-if="range.kind === 'absolute'" class="reset-zoom-btn" @click="resetRange">
              Reset zoom
            </button>
          </div>
          <div class="chart-wrapper">
            <div class="chart-container"><canvas :id="'mem-' + cname"></canvas></div>
          </div>
        </div>
      </div>
    </div>

    <!-- Init container charts -->
    <template v-if="initContainers().length > 0">
      <h2 class="mt-3">Init containers</h2>
      <div v-for="cname in initContainers()" :key="cname" class="card">
        <div class="card-header">
          <h2>
            Init container: <code>{{ cname }}</code>
          </h2>
          <span class="badge">init</span>
        </div>
        <div class="chart-grid">
          <div>
            <div class="chart-head">
              <div class="section-label">CPU Usage (cores)</div>
              <button v-if="range.kind === 'absolute'" class="reset-zoom-btn" @click="resetRange">
                Reset zoom
              </button>
            </div>
            <div class="chart-wrapper">
              <div class="chart-container"><canvas :id="'cpu-' + cname"></canvas></div>
            </div>
          </div>
          <div>
            <div class="chart-head">
              <div class="section-label" style="display: inline-flex; align-items: center">
                Memory Usage (MiB)
                <span v-if="(oomByContainer()[cname] || []).length > 0" class="oom-legend">
                  <span class="oom-marker"></span>
                  {{ oomByContainer()[cname].length }} OOM kill{{
                    oomByContainer()[cname].length > 1 ? 's' : ''
                  }}
                </span>
              </div>
              <button v-if="range.kind === 'absolute'" class="reset-zoom-btn" @click="resetRange">
                Reset zoom
              </button>
            </div>
            <div class="chart-wrapper">
              <div class="chart-container"><canvas :id="'mem-' + cname"></canvas></div>
            </div>
          </div>
        </div>
      </div>
    </template>

    <EmptyState
      v-if="containers().length === 0"
      icon="chart"
      title="No metrics yet"
      message="No metrics data available for this workload. Check that Prometheus is scraping and try again in a few minutes."
    />

    <div class="row mt-3">
      <RouterLink class="btn btn-secondary" :to="`/simulator/${namespace}/${kind}/${name}`">
        <svg
          viewBox="0 0 24 24"
          width="16"
          height="16"
          fill="none"
          stroke="currentColor"
          stroke-width="2"
        >
          <path
            d="M12 6V4m0 2a2 2 0 100 4m0-4a2 2 0 110 4m-6 8a2 2 0 100-4m0 4a2 2 0 110-4m0 4v2m0-6V4m6 6v10m6-2a2 2 0 100-4m0 4a2 2 0 110-4m0 4v2m0-6V4"
          />
        </svg>
        Open in Simulator
      </RouterLink>
    </div>
  </template>
</template>
