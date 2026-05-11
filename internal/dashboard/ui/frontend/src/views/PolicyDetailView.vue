<script setup lang="ts">
import { ref, onMounted, watch } from 'vue'
import { useRouter } from 'vue-router'
import {
  api,
  getTimeRangeStep,
  type PolicySpec,
  type PolicyWorkloadsData,
  type BatchSimulateData,
} from '../lib/api'
import { useAutoRefresh } from '../composables/useAutoRefresh'
import { useSorting } from '../composables/useSorting'
import { formatBytes } from '../lib/format'
import StatusBadge from '../components/StatusBadge.vue'
import ResourceDiff from '../components/ResourceDiff.vue'
import RiskBadge from '../components/RiskBadge.vue'
import TrendChart from '../components/TrendChart.vue'
import TimeRangeSelector from '../components/TimeRangeSelector.vue'
import YamlPreviewModal from '../components/YamlPreviewModal.vue'
import PageHeader from '../components/PageHeader.vue'
import LoadingState from '../components/LoadingState.vue'
import ErrorState from '../components/ErrorState.vue'
import EmptyState from '../components/EmptyState.vue'

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
    const step = getTimeRangeStep(timeWindow.value)
    const [p, w] = await Promise.all([
      api<PolicySpec>(`/api/policies/${props.name}?window=${timeWindow.value}&step=${step}`),
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
watch([nsFilter, page, timeWindow], load)

function rs() {
  return policy.value?.spec?.rightSizing?.resourcesConfigs || {}
}

function selector() {
  return policy.value?.spec?.selector || {}
}

function autoscalerCoord() {
  return policy.value?.spec?.rightSizing?.autoscalerCoordination || {}
}

function eviction() {
  return policy.value?.spec?.rightSizing?.update?.eviction || {}
}

function excludeInit(): boolean {
  return policy.value?.spec?.rightSizing?.excludeInitContainers === true
}

function updateTypes() {
  return policy.value?.spec?.rightSizing?.update?.types || policy.value?.update || {}
}

function limitsLabel(l?: { [k: string]: any }): string {
  if (!l) return 'default'
  if (l.equalsToRequest) return 'equalsToRequest'
  if (l.keepLimit) return 'keepLimit'
  if (l.keepLimitRequestRatio) return 'keepLimitRequestRatio'
  if (l.noLimit) return 'noLimit'
  if (typeof l.requestsLimitsRatio === 'number')
    return `requestsLimitsRatio: ${l.requestsLimitsRatio}`
  return 'default'
}

function matchExprs(): string[] {
  const ls = selector().labelSelector
  if (!ls?.matchExpressions) return []
  return ls.matchExpressions.map(
    (e) => `${e.key} ${e.operator}${e.values?.length ? ' [' + e.values.join(',') + ']' : ''}`,
  )
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
    { label: 'CPU saved', color: 'rgb(124, 58, 237)', points: cpu },
    { label: 'Mem saved', color: 'rgb(6, 182, 212)', points: mem },
  ]
}

function modeBadges(): string {
  const u = updateTypes()
  const parts: string[] = []
  if (u.deployment) parts.push(`Deploy:${u.deployment}`)
  if (u.statefulSet) parts.push(`STS:${u.statefulSet}`)
  if (u.daemonSet) parts.push(`DS:${u.daemonSet}`)
  if (u.cronJob) parts.push(`CJ:${u.cronJob}`)
  if (u.job) parts.push(`Job:${u.job}`)
  if (u.family) parts.push(`Family:${u.family}`)
  if (u.deploymentConfig) parts.push(`DC:${u.deploymentConfig}`)
  if (u.argoRollout) parts.push(`Rollout:${u.argoRollout}`)
  return parts.join(', ') || '-'
}

function renderYaml(p: typeof policy.value): string {
  if (!p) return ''
  return `# k8s.sustain.io/v1alpha1 Policy ${props.name}\n` + JSON.stringify(p.spec || {}, null, 2)
}
</script>

<template>
  <LoadingState v-if="loading" variant="kpi" message="Loading policy…" />
  <ErrorState v-else-if="error" :message="error" @retry="load" />
  <template v-else-if="policy && workloadData">
    <div class="breadcrumb">
      <a href="#" @click.prevent="router.push('/policies')">Policies</a><span>/</span
      ><span>{{ name }}</span>
    </div>

    <PageHeader :title="name" subtitle="Policy configuration and matched workloads">
      <template #actions>
        <TimeRangeSelector v-model="timeWindow" />
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
        <div class="stat-label">CPU saved</div>
        <div class="stat-value text-success">{{ (policy.cpuSavingsCores || 0).toFixed(2) }}c</div>
      </div>
      <div class="stat-card">
        <div class="stat-label">Memory saved</div>
        <div class="stat-value text-success">
          {{ formatBytes(policy.memSavingsBytes || 0) }}
        </div>
      </div>
    </div>

    <div class="card">
      <div class="card-header">
        <h2>Configuration</h2>
        <button class="btn btn-secondary" @click="yamlOpen = true">View as YAML</button>
      </div>
      <div class="sim-grid">
        <div>
          <div class="section-label">CPU</div>
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
            <div class="rec-row">
              <span class="label">Keep request</span
              ><span class="value">{{ rs().cpu?.requests?.keepRequest ? 'yes' : 'no' }}</span>
            </div>
            <div class="rec-row">
              <span class="label">Limits</span
              ><span class="value">{{ limitsLabel(rs().cpu?.limits) }}</span>
            </div>
          </div>
        </div>
        <div>
          <div class="section-label">Memory</div>
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
            <div class="rec-row">
              <span class="label">Keep request</span
              ><span class="value">{{ rs().memory?.requests?.keepRequest ? 'yes' : 'no' }}</span>
            </div>
            <div class="rec-row">
              <span class="label">Limits</span
              ><span class="value">{{ limitsLabel(rs().memory?.limits) }}</span>
            </div>
          </div>
        </div>
      </div>
      <div class="mt-3">
        <div class="rec-row">
          <span class="label">Update mode</span>
          <span class="value">{{ modeBadges() }}</span>
        </div>
        <div class="rec-row">
          <span class="label">Ignore safe-to-evict annotations</span>
          <span class="value">{{
            eviction().ignoreAutoscalerSafeToEvictAnnotations ? 'yes' : 'no'
          }}</span>
        </div>
        <div class="rec-row">
          <span class="label">Exclude init containers</span>
          <span class="value">{{ excludeInit() ? 'yes' : 'no' }}</span>
        </div>
        <div class="rec-row">
          <span class="label">Autoscaler coordination</span>
          <span class="value">
            {{ autoscalerCoord().enabled ? 'enabled' : 'disabled' }}
            <template v-if="autoscalerCoord().replicaBudgetAnchor !== undefined">
              · replicaBudgetAnchor: {{ autoscalerCoord().replicaBudgetAnchor }}
            </template>
          </span>
        </div>
      </div>
    </div>

    <div class="card">
      <div class="card-header">
        <h2>Selector</h2>
      </div>
      <div class="rec-row">
        <span class="label">Namespaces</span>
        <span class="value">
          <template v-if="(selector().namespaces || []).length">
            <span
              v-for="ns in selector().namespaces"
              :key="ns"
              class="badge badge-blue"
              style="margin-right: 4px"
              >{{ ns }}</span
            >
          </template>
          <template v-else>all namespaces</template>
        </span>
      </div>
      <div class="rec-row">
        <span class="label">matchLabels</span>
        <span class="value">
          <template
            v-if="
              selector().labelSelector?.matchLabels &&
              Object.keys(selector().labelSelector!.matchLabels!).length
            "
          >
            <code
              v-for="(v, k) in selector().labelSelector!.matchLabels"
              :key="k"
              style="margin-right: 6px"
              >{{ k }}={{ v }}</code
            >
          </template>
          <template v-else>-</template>
        </span>
      </div>
      <div class="rec-row">
        <span class="label">matchExpressions</span>
        <span class="value">
          <template v-if="matchExprs().length">
            <code v-for="e in matchExprs()" :key="e" style="margin-right: 6px">{{ e }}</code>
          </template>
          <template v-else>-</template>
        </span>
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
      <EmptyState v-else compact icon="chart" message="Insufficient data — check back in 24h." />
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
          <button class="btn btn-primary btn-sm" @click="runBatchSimulate">Simulate All</button>
        </div>
      </div>

      <EmptyState
        v-if="sortedWorkloads().length === 0"
        compact
        message="No workloads matched by this policy yet."
      />
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
    <LoadingState v-if="batchLoading" message="Simulating all workloads…" />
    <ErrorState v-else-if="batchError" :message="batchError" @retry="runBatchSimulate" />
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
