<script setup lang="ts">
import { ref, onMounted, onUnmounted, watch, nextTick } from 'vue'
import { useRouter } from 'vue-router'
import {
  api,
  defaultTimeRange,
  getTimeRangeStep,
  type MetricsData,
  type RecommendationsData,
  type RecommendationContainer,
  type WorkloadDetailSnapshot,
  type CoordinationFactors,
} from '../lib/api'
import { parseCPUQuantity, parseMemoryQuantity, parseStepToMs, timeAgo } from '../lib/format'
import {
  createTimeSeriesChart,
  destroyAllCharts,
  syncZoom,
  resetZoom,
  groupOOMEventsByContainer,
  type ExtraSeries,
  type ChartAnnotation,
} from '../lib/chart'
import { useAutoRefresh } from '../composables/useAutoRefresh'
import { useApi } from '../composables/useApi'
import TimeRangeSelector from '../components/TimeRangeSelector.vue'
import ResourceDiff from '../components/ResourceDiff.vue'
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

const router = useRouter()
const loading = ref(true)
const error = ref('')
const timeRange = ref(defaultTimeRange)
const metrics = ref<MetricsData | null>(null)
const recs = ref<RecommendationsData | null>(null)

const snapshot = useApi<WorkloadDetailSnapshot>(() =>
  api<WorkloadDetailSnapshot>(`/api/workloads/${props.namespace}/${props.kind}/${props.name}`),
)

async function load() {
  const step = getTimeRangeStep(timeRange.value)
  try {
    const [m, r] = await Promise.all([
      api<MetricsData>(
        `/api/workloads/${props.namespace}/${props.kind}/${props.name}/metrics?window=${timeRange.value}&step=${step}`,
      ),
      api<RecommendationsData>(
        `/api/workloads/${props.namespace}/${props.kind}/${props.name}/recommendations?window=${timeRange.value}&step=${step}`,
      ),
      snapshot.run(),
    ])
    metrics.value = m
    recs.value = r
    error.value = ''
    loading.value = false
    await nextTick()
    renderCharts()
  } catch (e: any) {
    error.value = e.message
    loading.value = false
  }
}

const { enabled: autoRefresh, toggle: toggleAutoRefresh } = useAutoRefresh(load)

onMounted(load)
onUnmounted(destroyAllCharts)

watch(timeRange, () => {
  loading.value = true
  destroyAllCharts()
  load()
})

function containers(): string[] {
  const s = new Set<string>()
  if (metrics.value?.cpu) Object.keys(metrics.value.cpu).forEach((k) => s.add(k))
  if (metrics.value?.memory) Object.keys(metrics.value.memory).forEach((k) => s.add(k))
  return Array.from(s)
}

function initContainerSet(): Set<string> {
  const names = metrics.value?.initContainers ?? recs.value?.initContainers ?? []
  return new Set(names)
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
  const all = recs.value?.containers || {}
  return Object.entries(all).filter(([name]) => !inits.has(name))
}

function initRecContainers(): [string, RecommendationContainer][] {
  const inits = initContainerSet()
  const all = recs.value?.containers || {}
  return Object.entries(all).filter(([name]) => inits.has(name))
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
  const cpuRecSeries = recs.value?.cpuRecommendations || {}
  const memRecSeries = recs.value?.memoryRecommendations || {}
  const ooms = oomByContainer()
  const stepMs = parseStepToMs(getTimeRangeStep(timeRange.value))

  containers().forEach((cname) => {
    const res = resources[cname] || {}

    // CPU chart
    if (metrics.value!.cpu?.[cname]) {
      const cpuAnnotations: ChartAnnotation[] = []
      const cpuExtra: ExtraSeries[] = []
      if (cpuRequests[cname]?.length) {
        cpuExtra.push({
          data: cpuRequests[cname],
          label: 'Request',
          color: '#f59e0b',
          dash: [4, 4],
        })
      } else if (res.cpuRequest) {
        cpuAnnotations.push({
          value: parseCPUQuantity(res.cpuRequest),
          label: 'Request: ' + res.cpuRequest,
          color: '#f59e0b',
          dash: [4, 4],
        })
      }
      if (cpuLimits[cname]?.length) {
        cpuExtra.push({
          data: cpuLimits[cname],
          label: 'Limit',
          color: '#f97316',
          dash: [2, 2],
        })
      } else if (res.cpuLimit) {
        cpuAnnotations.push({
          value: parseCPUQuantity(res.cpuLimit),
          label: 'Limit: ' + res.cpuLimit,
          color: '#f97316',
          dash: [2, 2],
        })
      }
      if (recs.value?.automated && cpuRecSeries[cname]?.length) {
        cpuExtra.push({
          data: cpuRecSeries[cname],
          label: 'Recommendation',
          color: '#ef4444',
          dash: [8, 4],
          stepped: false,
        })
      }
      createTimeSeriesChart('cpu-' + cname, metrics.value!.cpu[cname], {
        label: 'CPU Usage',
        color: 'rgb(124, 58, 237)',
        unit: 'cores',
        yFormat: (v) => v.toFixed(3),
        annotations: cpuAnnotations,
        extraSeries: cpuExtra,
        onZoomComplete: syncZoom,
        stepMs,
      })
    }

    // Memory chart
    if (metrics.value!.memory?.[cname]) {
      const memAnnotations: ChartAnnotation[] = []
      const memExtra: ExtraSeries[] = []
      if (memoryRequests[cname]?.length) {
        memExtra.push({
          data: memoryRequests[cname],
          label: 'Request',
          color: '#f59e0b',
          dash: [4, 4],
        })
      } else if (res.memoryRequest) {
        memAnnotations.push({
          value: parseMemoryQuantity(res.memoryRequest),
          label: 'Request: ' + res.memoryRequest,
          color: '#f59e0b',
          dash: [4, 4],
        })
      }
      if (memoryLimits[cname]?.length) {
        memExtra.push({
          data: memoryLimits[cname],
          label: 'Limit',
          color: '#f97316',
          dash: [2, 2],
        })
      } else if (res.memoryLimit) {
        memAnnotations.push({
          value: parseMemoryQuantity(res.memoryLimit),
          label: 'Limit: ' + res.memoryLimit,
          color: '#f97316',
          dash: [2, 2],
        })
      }
      if (recs.value?.automated && memRecSeries[cname]?.length) {
        memExtra.push({
          data: memRecSeries[cname],
          label: 'Recommendation',
          color: '#ef4444',
          dash: [8, 4],
          stepped: false,
        })
      }
      createTimeSeriesChart('mem-' + cname, metrics.value!.memory[cname], {
        label: 'Memory Usage',
        color: '#06b6d4',
        unit: 'MiB',
        transform: (v) => v / (1024 * 1024),
        yFormat: (v) => v.toFixed(0),
        annotations: memAnnotations,
        extraSeries: memExtra,
        oomEvents: ooms[cname] || [],
        onZoomComplete: syncZoom,
        stepMs,
      })
    }
  })
}

function snapshotRiskState(): 'safe' | 'drifted' | 'at-risk' | 'blocked' | '' {
  const s = snapshot.data.value
  if (!s) return ''
  if (s.blocked) return 'blocked'
  if (s.oom24h > 0) return 'at-risk'
  if (s.driftPercent > 10) return 'drifted'
  return 'safe'
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
  <template v-else-if="metrics && recs">
    <div class="breadcrumb">
      <a href="#" @click.prevent="router.push('/workloads')">Workloads</a><span>/</span
      ><span>{{ name }}</span>
    </div>

    <PageHeader :title="name">
      <template #title>
        <span
          class="kind-badge"
          :class="'kind-' + kind"
          style="font-size: 14px; margin-right: 4px"
          >{{ kind }}</span
        >
        {{ name }}
      </template>
      <template #subtitle>
        {{ namespace }} &mdash; {{ containers().length }} container(s) &mdash;
        <template v-if="recs.automated">
          <span class="badge badge-green">Automated</span>
          <a href="#" @click.prevent="router.push(`/policies/${recs.policyName}`)">{{
            recs.policyName
          }}</a>
        </template>
        <span v-else class="badge badge-dim">Manual</span>
      </template>
      <template #meta>
        <RiskBadge v-if="snapshotRiskState()" :state="snapshotRiskState() as any" />
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
        <TimeRangeSelector v-model="timeRange" />
        <label class="auto-refresh">
          <input
            type="checkbox"
            :checked="autoRefresh"
            @change="toggleAutoRefresh(($event.target as HTMLInputElement).checked)"
          />
          Auto-refresh (30s)
        </label>
      </template>
    </PageHeader>

    <!-- Status snapshot -->
    <div class="card">
      <div class="card-header"><h2>Status</h2></div>
      <div class="stats-row">
        <KpiCard label="Mode" :value="snapshot.data.value?.updateMode || '-'" />
        <KpiCard
          label="Last recycled"
          :value="
            snapshot.data.value?.lastRecycledAt
              ? timeAgo(snapshot.data.value.lastRecycledAt)
              : 'never'
          "
        />
        <KpiCard
          label="Drift"
          :value="(snapshot.data.value?.driftPercent || 0).toFixed(1) + '%'"
          :tone="snapshot.data.value && snapshot.data.value.driftPercent > 10 ? 'warn' : 'neutral'"
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

    <!-- Recommendations -->
    <div v-if="recs.automated && regularRecContainers().length > 0" class="card">
      <div class="card-header"><h2>Recommendations</h2></div>
      <div class="container-grid">
        <div v-for="[cname, rec] in regularRecContainers()" :key="cname" class="container-card">
          <h4>{{ cname }}</h4>
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
        </div>
      </div>
    </div>

    <!-- Init container recommendations -->
    <div v-if="recs.automated && initRecContainers().length > 0" class="card">
      <div class="card-header">
        <h2>Init container recommendations</h2>
        <span class="badge">init</span>
      </div>
      <div class="container-grid">
        <div v-for="[cname, rec] in initRecContainers()" :key="cname" class="container-card">
          <h4>{{ cname }}</h4>
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
          <div class="section-label">CPU Usage (cores)</div>
          <div class="chart-wrapper">
            <button
              class="reset-zoom-btn"
              :id="'rz-cpu-' + cname"
              @click="resetZoom('cpu-' + cname)"
            >
              Reset zoom
            </button>
            <div class="chart-container"><canvas :id="'cpu-' + cname"></canvas></div>
          </div>
        </div>
        <div>
          <div class="section-label" style="display: inline-flex; align-items: center">
            Memory Usage (MiB)
            <span v-if="(oomByContainer()[cname] || []).length > 0" class="oom-legend">
              <span class="oom-marker"></span>
              {{ oomByContainer()[cname].length }} OOM kill{{
                oomByContainer()[cname].length > 1 ? 's' : ''
              }}
            </span>
          </div>
          <div class="chart-wrapper">
            <button
              class="reset-zoom-btn"
              :id="'rz-mem-' + cname"
              @click="resetZoom('mem-' + cname)"
            >
              Reset zoom
            </button>
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
            <div class="section-label">CPU Usage (cores)</div>
            <div class="chart-wrapper">
              <button
                class="reset-zoom-btn"
                :id="'rz-cpu-' + cname"
                @click="resetZoom('cpu-' + cname)"
              >
                Reset zoom
              </button>
              <div class="chart-container"><canvas :id="'cpu-' + cname"></canvas></div>
            </div>
          </div>
          <div>
            <div class="section-label" style="display: inline-flex; align-items: center">
              Memory Usage (MiB)
              <span v-if="(oomByContainer()[cname] || []).length > 0" class="oom-legend">
                <span class="oom-marker"></span>
                {{ oomByContainer()[cname].length }} OOM kill{{
                  oomByContainer()[cname].length > 1 ? 's' : ''
                }}
              </span>
            </div>
            <div class="chart-wrapper">
              <button
                class="reset-zoom-btn"
                :id="'rz-mem-' + cname"
                @click="resetZoom('mem-' + cname)"
              >
                Reset zoom
              </button>
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
      <button
        class="btn btn-secondary"
        @click="router.push(`/simulator/${namespace}/${kind}/${name}`)"
      >
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
      </button>
    </div>
  </template>
</template>
