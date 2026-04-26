<script setup lang="ts">
import { ref, onMounted, watch } from 'vue'
import { useRouter } from 'vue-router'
import { api, type PolicySpec, type PolicyWorkloadsData, type BatchSimulateData } from '../lib/api'
import { useAutoRefresh } from '../composables/useAutoRefresh'
import { useSorting } from '../composables/useSorting'
import StatusBadge from '../components/StatusBadge.vue'
import ResourceDiff from '../components/ResourceDiff.vue'
import RiskBadge from '../components/RiskBadge.vue'
import TrendChart from '../components/TrendChart.vue'
import TimeRangeSelector from '../components/TimeRangeSelector.vue'
import YamlPreviewModal from '../components/YamlPreviewModal.vue'

const props = defineProps<{ name: string }>()
const router = useRouter()
const loading = ref(true)
const error = ref('')
const policy = ref<PolicySpec | null>(null)
const workloadData = ref<PolicyWorkloadsData | null>(null)
const nsFilter = ref('')
const page = ref(1)
const batchLoading = ref(false)
const batchData = ref<BatchSimulateData | null>(null)
const batchError = ref('')
const timeWindow = ref('168h')
const yamlOpen = ref(false)

const { sort, sortArrow, applySorting } = useSorting('policyWorkloads')

async function load() {
  try {
    const [p, w] = await Promise.all([
      api<PolicySpec>(`/api/policies/${props.name}`),
      api<PolicyWorkloadsData>(
        `/api/policies/${props.name}/workloads?page=${page.value}&pageSize=50${nsFilter.value ? '&namespace=' + encodeURIComponent(nsFilter.value) : ''}`,
      ),
    ])
    policy.value = p
    workloadData.value = w
    error.value = ''
  } catch (e: any) {
    error.value = e.message
  } finally {
    loading.value = false
  }
}

const { enabled: autoRefresh, toggle: toggleAutoRefresh } = useAutoRefresh(load)

onMounted(load)
watch([nsFilter, page], load)

function rs() {
  return policy.value?.spec?.rightSizing?.resourcesConfigs || {}
}

function sortedWorkloads() {
  return applySorting(workloadData.value?.items || [])
}

function totalPages() {
  if (!workloadData.value) return 1
  return Math.ceil(workloadData.value.total / (workloadData.value.pageSize || 50))
}

async function runBatchSimulate() {
  batchLoading.value = true
  batchError.value = ''
  try {
    batchData.value = await api<BatchSimulateData>(`/api/policies/${props.name}/batch-simulate`)
  } catch (e: any) {
    batchError.value = e.message
  } finally {
    batchLoading.value = false
  }
}

function savingsClass(millis: number): string {
  return millis > 0 ? 'savings-positive' : millis < 0 ? 'savings-negative' : ''
}

function effectivenessSeries() {
  const e = policy.value?.effectivenessSeries
  if (!e) return []
  const cpu = e.cpu || []
  const mem = e.memory || []
  if (cpu.length === 0 && mem.length === 0) return []
  return [
    { label: 'CPU saved', color: '#6366f1', points: cpu },
    { label: 'Mem saved', color: '#06b6d4', points: mem },
  ]
}

function modeBadges(): string {
  const u = policy.value?.spec?.update
  if (!u) return '-'
  const parts: string[] = []
  if (u.deployment) parts.push(`Deploy:${u.deployment}`)
  if (u.statefulSet) parts.push(`STS:${u.statefulSet}`)
  if (u.daemonSet) parts.push(`DS:${u.daemonSet}`)
  if (u.cronJob) parts.push(`CJ:${u.cronJob}`)
  return parts.join(', ') || '-'
}

function renderYaml(p: typeof policy.value): string {
  if (!p) return ''
  return `# k8s.sustain.io/v1alpha1 Policy ${props.name}\n` + JSON.stringify(p.spec || {}, null, 2)
}
</script>

<template>
  <div v-if="loading" class="loading">
    <div class="spinner"></div>
    Loading policy...
  </div>
  <div v-else-if="error" class="card">
    <p style="color: var(--red)">Error: {{ error }}</p>
  </div>
  <template v-else-if="policy && workloadData">
    <div class="breadcrumb">
      <a href="#" @click.prevent="router.push('/policies')">Policies</a><span>/</span
      ><span>{{ name }}</span>
    </div>

    <div
      class="page-header"
      style="display: flex; align-items: flex-start; justify-content: space-between"
    >
      <div>
        <h1>{{ name }}</h1>
        <p>Policy configuration and matched workloads</p>
      </div>
      <div class="time-range-bar">
        <TimeRangeSelector v-model="timeWindow" />
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

    <div class="stats-row">
      <div class="stat-card">
        <div class="stat-label">Status</div>
        <div class="stat-value"><StatusBadge :conditions="policy.conditions" /></div>
      </div>
      <div class="stat-card">
        <div class="stat-label">Matched Workloads</div>
        <div class="stat-value">{{ workloadData.total }}</div>
      </div>
      <div class="stat-card">
        <div class="stat-label">CPU Percentile</div>
        <div class="stat-value">{{ rs().cpu?.requests?.percentile || 95 }}th</div>
      </div>
      <div class="stat-card">
        <div class="stat-label">Memory Percentile</div>
        <div class="stat-value">{{ rs().memory?.requests?.percentile || 95 }}th</div>
      </div>
    </div>

    <div class="card">
      <div class="card-header">
        <h2>Configuration</h2>
        <button class="btn btn-secondary" @click="yamlOpen = true">View as YAML</button>
      </div>
      <div class="sim-grid">
        <div>
          <h3 style="font-size: 13px; color: var(--text-dim); margin-bottom: 8px">CPU</h3>
          <div class="rec-card">
            <div class="rec-row">
              <span class="label">Window</span
              ><span class="value">{{ rs().cpu?.window || '168h' }}</span>
            </div>
            <div class="rec-row">
              <span class="label">Percentile</span
              ><span class="value">{{ rs().cpu?.requests?.percentile || 95 }}%</span>
            </div>
            <div class="rec-row">
              <span class="label">Headroom</span
              ><span class="value">{{ rs().cpu?.requests?.headroom || 0 }}%</span>
            </div>
            <div class="rec-row">
              <span class="label">Min</span
              ><span class="value">{{ rs().cpu?.requests?.minAllowed || '-' }}</span>
            </div>
            <div class="rec-row">
              <span class="label">Max</span
              ><span class="value">{{ rs().cpu?.requests?.maxAllowed || '-' }}</span>
            </div>
          </div>
        </div>
        <div>
          <h3 style="font-size: 13px; color: var(--text-dim); margin-bottom: 8px">Memory</h3>
          <div class="rec-card">
            <div class="rec-row">
              <span class="label">Window</span
              ><span class="value">{{ rs().memory?.window || '168h' }}</span>
            </div>
            <div class="rec-row">
              <span class="label">Percentile</span
              ><span class="value">{{ rs().memory?.requests?.percentile || 95 }}%</span>
            </div>
            <div class="rec-row">
              <span class="label">Headroom</span
              ><span class="value">{{ rs().memory?.requests?.headroom || 0 }}%</span>
            </div>
            <div class="rec-row">
              <span class="label">Min</span
              ><span class="value">{{ rs().memory?.requests?.minAllowed || '-' }}</span>
            </div>
            <div class="rec-row">
              <span class="label">Max</span
              ><span class="value">{{ rs().memory?.requests?.maxAllowed || '-' }}</span>
            </div>
          </div>
        </div>
      </div>
      <div style="margin-top: 16px">
        <div class="rec-row">
          <span class="label">Update mode</span>
          <span class="value">{{ modeBadges() }}</span>
        </div>
      </div>
    </div>

    <div class="card">
      <div class="card-header">
        <h2>Effectiveness over time</h2>
        <TimeRangeSelector v-model="timeWindow" />
      </div>
      <TrendChart
        v-if="effectivenessSeries().length"
        :series="effectivenessSeries()"
        unit=""
        :height="220"
      />
      <div v-else class="empty-state"><p>Insufficient data — check back in 24h.</p></div>
    </div>

    <div class="card">
      <div class="card-header">
        <h2>Matched Workloads</h2>
        <div class="filter-bar">
          <select
            v-if="(workloadData.namespaces || []).length > 1"
            v-model="nsFilter"
            @change="page = 1"
          >
            <option value="">All namespaces</option>
            <option v-for="ns in workloadData.namespaces?.sort()" :key="ns" :value="ns">
              {{ ns }}
            </option>
          </select>
          <span class="badge badge-blue">{{ workloadData.total }} workloads</span>
          <button
            class="btn btn-primary"
            style="padding: 6px 14px; font-size: 13px"
            @click="runBatchSimulate"
          >
            Simulate All
          </button>
        </div>
      </div>

      <div v-if="sortedWorkloads().length === 0" class="empty-state">
        <p>No workloads matched by this policy yet.</p>
      </div>
      <template v-else>
        <div class="table-wrap">
          <table>
            <thead>
              <tr>
                <th class="sort-header" @click="sort('namespace')">
                  Namespace<span v-html="sortArrow('namespace')"></span>
                </th>
                <th class="sort-header" @click="sort('kind')">
                  Kind<span v-html="sortArrow('kind')"></span>
                </th>
                <th class="sort-header" @click="sort('name')">
                  Name<span v-html="sortArrow('name')"></span>
                </th>
                <th>Risk</th>
                <th class="sort-header" @click="sort('driftPercent')">
                  Drift<span v-html="sortArrow('driftPercent')"></span>
                </th>
                <th>Containers</th>
                <th>CPU Req</th>
                <th>Mem Req</th>
              </tr>
            </thead>
            <tbody>
              <tr
                v-for="w in sortedWorkloads()"
                :key="w.namespace + '/' + w.name"
                @click="router.push(`/workloads/${w.namespace}/${w.kind}/${w.name}`)"
              >
                <td style="color: var(--text-dim)">{{ w.namespace }}</td>
                <td>
                  <span class="kind-badge" :class="'kind-' + w.kind">{{ w.kind }}</span>
                </td>
                <td style="font-weight: 600">{{ w.name }}</td>
                <td><RiskBadge :state="w.riskState" /></td>
                <td>
                  <code v-if="w.driftPercent">{{ w.driftPercent.toFixed(1) }}%</code
                  ><span v-else style="color: var(--text-dim)">-</span>
                </td>
                <td style="color: var(--text-dim)">
                  {{ w.containers.map((c) => c.name).join(', ') }}
                </td>
                <td>
                  <code>{{ w.containers.map((c) => c.cpuRequest || '-').join(', ') }}</code>
                </td>
                <td>
                  <code>{{ w.containers.map((c) => c.memoryRequest || '-').join(', ') }}</code>
                </td>
              </tr>
            </tbody>
          </table>
        </div>
        <div v-if="totalPages() > 1" class="pagination">
          <button :disabled="page <= 1" @click="page--">Previous</button>
          <span>Page {{ page }} of {{ totalPages() }}</span>
          <button :disabled="page >= totalPages()" @click="page++">Next</button>
        </div>
      </template>
    </div>

    <!-- Batch simulation results -->
    <div v-if="batchLoading" class="loading">
      <div class="spinner"></div>
      Simulating all workloads...
    </div>
    <div v-else-if="batchError" class="card">
      <p style="color: var(--red)">Error: {{ batchError }}</p>
    </div>
    <div v-else-if="batchData" class="card">
      <div class="card-header"><h2>Batch Simulation Results</h2></div>
      <div class="stats-row" style="margin-bottom: 16px">
        <div class="stat-card">
          <div class="stat-label">CPU Savings</div>
          <div class="stat-value" :class="savingsClass(batchData.cpu.savingsMillis)">
            {{ batchData.cpu.savingsFormatted }}
          </div>
          <div class="stat-detail">
            {{ batchData.cpu.currentFormatted }} &rarr; {{ batchData.cpu.recommendedFormatted }}
          </div>
        </div>
        <div class="stat-card">
          <div class="stat-label">Memory Savings</div>
          <div class="stat-value" :class="savingsClass(batchData.memory.savingsMillis)">
            {{ batchData.memory.savingsFormatted }}
          </div>
          <div class="stat-detail">
            {{ batchData.memory.currentFormatted }} &rarr;
            {{ batchData.memory.recommendedFormatted }}
          </div>
        </div>
      </div>
      <div class="table-wrap">
        <table>
          <thead>
            <tr>
              <th>Namespace</th>
              <th>Kind</th>
              <th>Name</th>
              <th>Container</th>
              <th>CPU</th>
              <th>Memory</th>
            </tr>
          </thead>
          <tbody>
            <template v-for="w in batchData.workloads" :key="w.namespace + '/' + w.name">
              <tr v-if="w.error">
                <td style="color: var(--text-dim)">{{ w.namespace }}</td>
                <td>
                  <span class="kind-badge" :class="'kind-' + w.kind">{{ w.kind }}</span>
                </td>
                <td>{{ w.name }}</td>
                <td colspan="3" style="color: var(--red)">{{ w.error }}</td>
              </tr>
              <template v-else>
                <tr v-for="(c, cname, idx) in w.containers" :key="cname">
                  <td
                    v-if="idx === 0"
                    :rowspan="Object.keys(w.containers!).length"
                    style="color: var(--text-dim)"
                  >
                    {{ w.namespace }}
                  </td>
                  <td v-if="idx === 0" :rowspan="Object.keys(w.containers!).length">
                    <span class="kind-badge" :class="'kind-' + w.kind">{{ w.kind }}</span>
                  </td>
                  <td
                    v-if="idx === 0"
                    :rowspan="Object.keys(w.containers!).length"
                    style="font-weight: 600"
                  >
                    {{ w.name }}
                  </td>
                  <td style="color: var(--accent-light); font-family: monospace; font-size: 12px">
                    {{ cname }}
                  </td>
                  <td>
                    <ResourceDiff
                      :current="c.currentCpu"
                      :recommended="c.recommendedCpu"
                      resource-type="cpu"
                    />
                  </td>
                  <td>
                    <ResourceDiff
                      :current="c.currentMemory"
                      :recommended="c.recommendedMemory"
                      resource-type="memory"
                    />
                  </td>
                </tr>
              </template>
            </template>
          </tbody>
        </table>
      </div>
    </div>
  </template>

  <YamlPreviewModal
    :open="yamlOpen"
    title="Policy spec"
    :yaml="renderYaml(policy)"
    @close="yamlOpen = false"
  />
</template>
