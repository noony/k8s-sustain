<script setup lang="ts">
import { ref, onMounted, onUnmounted, nextTick, watch } from 'vue'
import {
  api,
  defaultTimeRange,
  getTimeRangeStep,
  type PolicySummary,
  type PolicySpec,
  type SimulationResult,
  type SimulateRequest,
} from '../lib/api'
import { parseCPUQuantity, parseMemoryQuantity, downloadFile } from '../lib/format'
import {
  createTimeSeriesChart,
  destroyAllCharts,
  syncZoom,
  resetZoom,
  type ExtraSeries,
  type ChartAnnotation,
} from '../lib/chart'
import TimeRangeSelector from '../components/TimeRangeSelector.vue'
import ResourceDiff from '../components/ResourceDiff.vue'

const props = defineProps<{
  namespace?: string
  kind?: string
  name?: string
}>()

// Form state
const simNs = ref(props.namespace || '')
const simKind = ref(props.kind || 'Deployment')
const simName = ref(props.name || '')
const timeRange = ref(defaultTimeRange)
const cpuWindow = ref(defaultTimeRange)
const memWindow = ref(defaultTimeRange)
const cpuPct = ref(95)
const cpuHr = ref(0)
const cpuMin = ref('')
const cpuMax = ref('')
const memPct = ref(95)
const memHr = ref(0)
const memMin = ref('')
const memMax = ref('')

// State
const policies = ref<PolicySummary[]>([])
const selectedPolicy = ref('')
const simData = ref<SimulationResult | null>(null)
const simError = ref('')
const firstRun = ref(true)
let debounceTimer: ReturnType<typeof setTimeout> | null = null
let requestId = 0

async function loadPolicies() {
  try {
    policies.value = await api<PolicySummary[]>('/api/policies')
  } catch {
    policies.value = []
  }
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

    scheduleSimulation()
  } catch (e) {
    console.error('Failed to load policy:', e)
  }
}

function scheduleSimulation() {
  if (debounceTimer) clearTimeout(debounceTimer)
  debounceTimer = setTimeout(runSimulation, 400)
}

async function runSimulation() {
  if (!simNs.value || !simName.value) {
    simData.value = null
    destroyAllCharts()
    return
  }

  const id = ++requestId

  const body: SimulateRequest = {
    namespace: simNs.value,
    ownerKind: simKind.value,
    ownerName: simName.value,
    window: timeRange.value,
    step: getTimeRangeStep(timeRange.value),
    cpu: { percentile: cpuPct.value, headroom: cpuHr.value, window: cpuWindow.value },
    memory: { percentile: memPct.value, headroom: memHr.value, window: memWindow.value },
  }
  if (cpuMin.value) body.cpu.minAllowed = cpuMin.value
  if (cpuMax.value) body.cpu.maxAllowed = cpuMax.value
  if (memMin.value) body.memory.minAllowed = memMin.value
  if (memMax.value) body.memory.maxAllowed = memMax.value

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

    await nextTick()
    renderCharts(data)
  } catch (e: any) {
    if (id === requestId) {
      simError.value = e.message
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
        color: '#6366f1',
        unit: 'cores',
        yFormat: (v) => v.toFixed(3),
        annotations: cpuAnnotations,
        extraSeries: cpuExtra,
        onZoomComplete: syncZoom,
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
        onZoomComplete: syncZoom,
      })
    }
  })
}

function exportYAML() {
  if (!simData.value) return
  const containers = Object.entries(simData.value.containers)
  const resourceLines = containers
    .map(([cname, rec]) => {
      const parts = [`        - name: ${cname}\n          resources:\n            requests:`]
      if (rec.cpuRequest) parts.push(`              cpu: "${rec.cpuRequest}"`)
      if (rec.memoryRequest) parts.push(`              memory: "${rec.memoryRequest}"`)
      return parts.join('\n')
    })
    .join('\n')

  const yaml = `# k8s-sustain simulation recommendation
# Workload: ${simNs.value}/${simKind.value}/${simName.value}
# Window: ${timeRange.value} | CPU P${cpuPct.value}+${cpuHr.value}% | Mem P${memPct.value}+${memHr.value}%
apiVersion: apps/v1
kind: ${simKind.value}
metadata:
  name: ${simName.value}
  namespace: ${simNs.value}
spec:
  template:
    spec:
      containers:
${resourceLines}
`
  downloadFile(simName.value + '-recommendations.yaml', yaml, 'text/yaml')
}

function exportCSV() {
  if (!simData.value) return
  const rows = [['Container', 'CPU Request', 'Memory Request']]
  for (const [name, rec] of Object.entries(simData.value.containers)) {
    rows.push([name, rec.cpuRequest || '', rec.memoryRequest || ''])
  }
  const csv = rows.map((r) => r.join(',')).join('\n')
  downloadFile(simName.value + '-recommendations.csv', csv, 'text/csv')
}

// Watch all form inputs for debounced auto-simulation
watch(
  [
    simNs,
    simKind,
    simName,
    timeRange,
    cpuWindow,
    memWindow,
    cpuPct,
    cpuHr,
    cpuMin,
    cpuMax,
    memPct,
    memHr,
    memMin,
    memMax,
  ],
  scheduleSimulation,
)

onMounted(async () => {
  await loadPolicies()
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
  <div class="page-header">
    <h1>Policy Simulator</h1>
    <p>Simulate policy parameter changes and preview their effect on historical metrics</p>
  </div>

  <!-- Workload Target -->
  <div class="card">
    <div class="card-header"><h2>Workload Target</h2></div>
    <div class="workload-selector">
      <div class="form-group">
        <label>Namespace</label>
        <input v-model="simNs" type="text" placeholder="default" />
      </div>
      <div class="form-group">
        <label>Kind</label>
        <select v-model="simKind">
          <option value="Deployment">Deployment</option>
          <option value="StatefulSet">StatefulSet</option>
          <option value="DaemonSet">DaemonSet</option>
          <option value="CronJob">CronJob</option>
        </select>
      </div>
      <div class="form-group">
        <label>Name</label>
        <input v-model="simName" type="text" placeholder="my-app" />
      </div>
      <div class="form-group">
        <label>Time range</label>
        <TimeRangeSelector v-model="timeRange" />
      </div>
    </div>
  </div>

  <!-- Configuration -->
  <div class="card">
    <div
      class="card-header"
      style="display: flex; align-items: center; justify-content: space-between"
    >
      <h2>Configuration</h2>
      <div class="form-group" style="margin: 0; display: flex; align-items: center; gap: 8px">
        <label style="margin: 0; white-space: nowrap; font-size: 12px">Load from policy</label>
        <select
          v-model="selectedPolicy"
          style="min-width: 160px"
          @change="loadPolicyConfig(selectedPolicy)"
        >
          <option value="">-- Select a policy --</option>
          <option v-for="p in policies" :key="p.name" :value="p.name">{{ p.name }}</option>
        </select>
      </div>
    </div>

    <div class="sim-grid">
      <!-- CPU -->
      <div class="sim-section">
        <h3><span style="color: var(--accent-light)">CPU</span> Configuration</h3>
        <div class="form-group">
          <label>Window</label>
          <TimeRangeSelector v-model="cpuWindow" />
        </div>
        <div class="form-group">
          <label>Percentile</label>
          <div class="slider-row">
            <input type="range" v-model.number="cpuPct" min="50" max="99" />
            <span class="slider-value">{{ cpuPct }}%</span>
          </div>
        </div>
        <div class="form-group">
          <label>Headroom</label>
          <div class="slider-row">
            <input type="range" v-model.number="cpuHr" min="0" max="100" />
            <span class="slider-value">{{ cpuHr }}%</span>
          </div>
        </div>
        <div class="form-group">
          <label>Min Allowed</label>
          <input v-model="cpuMin" type="text" placeholder="e.g. 50m" />
        </div>
        <div class="form-group">
          <label>Max Allowed</label>
          <input v-model="cpuMax" type="text" placeholder="e.g. 4000m" />
        </div>
      </div>

      <!-- Memory -->
      <div class="sim-section">
        <h3><span style="color: var(--cyan)">Memory</span> Configuration</h3>
        <div class="form-group">
          <label>Window</label>
          <TimeRangeSelector v-model="memWindow" />
        </div>
        <div class="form-group">
          <label>Percentile</label>
          <div class="slider-row">
            <input type="range" v-model.number="memPct" min="50" max="99" />
            <span class="slider-value">{{ memPct }}%</span>
          </div>
        </div>
        <div class="form-group">
          <label>Headroom</label>
          <div class="slider-row">
            <input type="range" v-model.number="memHr" min="0" max="100" />
            <span class="slider-value">{{ memHr }}%</span>
          </div>
        </div>
        <div class="form-group">
          <label>Min Allowed</label>
          <input v-model="memMin" type="text" placeholder="e.g. 64Mi" />
        </div>
        <div class="form-group">
          <label>Max Allowed</label>
          <input v-model="memMax" type="text" placeholder="e.g. 8Gi" />
        </div>
      </div>
    </div>
  </div>

  <!-- Results -->
  <div v-if="firstRun && !simData && !simError"></div>
  <div v-else-if="simError" class="card">
    <p style="color: var(--red)">Error: {{ simError }}</p>
  </div>
  <template v-else-if="simData">
    <div v-if="Object.keys(simData.containers).length === 0" class="card">
      <div class="empty-state">
        <p>
          No metrics data found for this workload. Make sure the namespace, kind, and name are
          correct.
        </p>
      </div>
    </div>
    <template v-else>
      <div class="card">
        <div class="card-header">
          <h2>Simulation Results</h2>
          <div style="display: flex; gap: 8px">
            <button class="btn btn-secondary" @click="exportYAML" title="Download as YAML patch">
              <svg
                viewBox="0 0 24 24"
                width="14"
                height="14"
                fill="none"
                stroke="currentColor"
                stroke-width="2"
              >
                <path d="M21 15v4a2 2 0 01-2 2H5a2 2 0 01-2-2v-4M7 10l5 5 5-5M12 15V3" />
              </svg>
              YAML
            </button>
            <button class="btn btn-secondary" @click="exportCSV" title="Download as CSV">
              <svg
                viewBox="0 0 24 24"
                width="14"
                height="14"
                fill="none"
                stroke="currentColor"
                stroke-width="2"
              >
                <path d="M21 15v4a2 2 0 01-2 2H5a2 2 0 01-2-2v-4M7 10l5 5 5-5M12 15V3" />
              </svg>
              CSV
            </button>
          </div>
        </div>
        <div class="rec-grid">
          <div v-for="(rec, cname) in simData.containers" :key="cname" class="rec-card">
            <h4>{{ cname }}</h4>
            <div class="rec-row">
              <span class="label">CPU Request</span>
              <ResourceDiff
                :current="(simData.resources || {})[cname as string]?.cpuRequest"
                :recommended="rec.cpuRequest"
                resource-type="cpu"
              />
            </div>
            <div class="rec-row">
              <span class="label">Memory Request</span>
              <ResourceDiff
                :current="(simData.resources || {})[cname as string]?.memoryRequest"
                :recommended="rec.memoryRequest"
                resource-type="memory"
              />
            </div>
          </div>
        </div>
      </div>

      <div v-for="cname in Object.keys(simData.containers)" :key="cname" class="card">
        <div class="card-header">
          <h2>
            Container: <code>{{ cname }}</code>
          </h2>
        </div>
        <div class="chart-grid">
          <div>
            <h3 style="font-size: 13px; color: var(--text-dim); margin-bottom: 8px">
              CPU Usage + Recommendation
            </h3>
            <div class="chart-wrapper">
              <button
                class="reset-zoom-btn"
                :id="'rz-simcpu-' + cname"
                @click="resetZoom('simcpu-' + cname)"
              >
                Reset zoom
              </button>
              <div class="chart-container"><canvas :id="'simcpu-' + cname"></canvas></div>
            </div>
          </div>
          <div>
            <h3 style="font-size: 13px; color: var(--text-dim); margin-bottom: 8px">
              Memory Usage + Recommendation
            </h3>
            <div class="chart-wrapper">
              <button
                class="reset-zoom-btn"
                :id="'rz-simmem-' + cname"
                @click="resetZoom('simmem-' + cname)"
              >
                Reset zoom
              </button>
              <div class="chart-container"><canvas :id="'simmem-' + cname"></canvas></div>
            </div>
          </div>
        </div>
      </div>
    </template>
  </template>
</template>
