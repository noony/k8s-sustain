<script setup lang="ts">
import { ref, onMounted, onUnmounted, watch, nextTick } from 'vue'
import { useRouter } from 'vue-router'
import {
  api,
  defaultTimeRange,
  getTimeRangeStep,
  type MetricsData,
  type RecommendationsData,
} from '../lib/api'
import { parseCPUQuantity, parseMemoryQuantity } from '../lib/format'
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
import TimeRangeSelector from '../components/TimeRangeSelector.vue'
import ResourceDiff from '../components/ResourceDiff.vue'

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

function oomByContainer() {
  return groupOOMEventsByContainer(metrics.value?.oomEvents)
}

function renderCharts() {
  destroyAllCharts()
  if (!metrics.value) return

  const resources = metrics.value.resources || {}
  const cpuRequests = metrics.value.cpuRequests || {}
  const memoryRequests = metrics.value.memoryRequests || {}
  const cpuRecSeries = recs.value?.cpuRecommendations || {}
  const memRecSeries = recs.value?.memoryRecommendations || {}
  const ooms = oomByContainer()

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
      if (res.cpuLimit)
        cpuAnnotations.push({
          value: parseCPUQuantity(res.cpuLimit),
          label: 'Limit: ' + res.cpuLimit,
          color: '#f97316',
          dash: [2, 2],
        })
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
        color: '#6366f1',
        unit: 'cores',
        yFormat: (v) => v.toFixed(3),
        annotations: cpuAnnotations,
        extraSeries: cpuExtra,
        onZoomComplete: syncZoom,
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
      if (res.memoryLimit)
        memAnnotations.push({
          value: parseMemoryQuantity(res.memoryLimit),
          label: 'Limit: ' + res.memoryLimit,
          color: '#f97316',
          dash: [2, 2],
        })
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
      })
    }
  })
}
</script>

<template>
  <div v-if="loading" class="loading">
    <div class="spinner"></div>
    Loading workload...
  </div>
  <div v-else-if="error" class="card">
    <p style="color: var(--red)">Error: {{ error }}</p>
  </div>
  <template v-else-if="metrics && recs">
    <div class="breadcrumb">
      <a href="#" @click.prevent="router.push('/workloads')">Workloads</a><span>/</span
      ><span>{{ name }}</span>
    </div>

    <div
      class="page-header"
      style="display: flex; align-items: flex-start; justify-content: space-between"
    >
      <div>
        <h1>
          <span
            class="kind-badge"
            :class="'kind-' + kind"
            style="font-size: 14px; margin-right: 4px"
            >{{ kind }}</span
          >
          {{ name }}
        </h1>
        <p>
          {{ namespace }} &mdash; {{ containers().length }} container(s) &mdash;
          <template v-if="recs.automated">
            <span class="badge badge-green">Automated</span>
            <a href="#" @click.prevent="router.push(`/policies/${recs.policyName}`)">{{
              recs.policyName
            }}</a>
          </template>
          <span v-else class="badge badge-dim">Manual</span>
        </p>
      </div>
      <div class="time-range-bar">
        <TimeRangeSelector v-model="timeRange" />
        <label class="auto-refresh">
          <input
            type="checkbox"
            :checked="autoRefresh"
            @change="toggleAutoRefresh(($event.target as HTMLInputElement).checked)"
          />
          Auto-refresh (30s)
        </label>
      </div>
    </div>

    <!-- Recommendations -->
    <div
      v-if="recs.automated && recs.containers && Object.keys(recs.containers).length > 0"
      class="card"
    >
      <div class="card-header"><h2>Recommendations</h2></div>
      <div class="container-grid">
        <div v-for="(rec, cname) in recs.containers" :key="cname" class="container-card">
          <h4>{{ cname }}</h4>
          <div class="resource-row">
            <span class="label">CPU Request</span>
            <ResourceDiff
              :current="(metrics.resources || {})[cname as string]?.cpuRequest"
              :recommended="rec.cpuRequest"
              resource-type="cpu"
            />
          </div>
          <div class="resource-row">
            <span class="label">Memory Request</span>
            <ResourceDiff
              :current="(metrics.resources || {})[cname as string]?.memoryRequest"
              :recommended="rec.memoryRequest"
              resource-type="memory"
            />
          </div>
        </div>
      </div>
    </div>

    <!-- Charts per container -->
    <div v-for="cname in containers()" :key="cname" class="card">
      <div class="card-header">
        <h2>
          Container: <code>{{ cname }}</code>
        </h2>
      </div>
      <div class="chart-grid">
        <div>
          <h3 style="font-size: 13px; color: var(--text-dim); margin-bottom: 8px">
            CPU Usage (cores)
          </h3>
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
          <h3 style="font-size: 13px; color: var(--text-dim); margin-bottom: 8px">
            Memory Usage (MiB)
            <span v-if="(oomByContainer()[cname] || []).length > 0" class="oom-legend">
              <span class="oom-marker"></span>
              {{ oomByContainer()[cname].length }} OOM kill{{
                oomByContainer()[cname].length > 1 ? 's' : ''
              }}
            </span>
          </h3>
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

    <div v-if="containers().length === 0" class="card">
      <div class="empty-state"><p>No metrics data available.</p></div>
    </div>

    <div style="margin-top: 16px">
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
