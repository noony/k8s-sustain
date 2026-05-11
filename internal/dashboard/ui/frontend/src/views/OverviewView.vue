<script setup lang="ts">
import { onMounted, computed, watch } from 'vue'
import { useRouter } from 'vue-router'
import {
  api,
  getTimeRangeStep,
  type SummaryV2,
  type TrendData,
  type AttentionRow,
  type ActivityItem,
  type WorkloadListData,
  type WorkloadItemV2,
} from '../lib/api'
import { useAutoRefresh } from '../composables/useAutoRefresh'
import { useApi } from '../composables/useApi'
import { usePrometheusTime } from '../composables/usePrometheusTime'
import KpiCard from '../components/KpiCard.vue'
import HeadroomBar from '../components/HeadroomBar.vue'
import AttentionQueue from '../components/AttentionQueue.vue'
import TrendChart from '../components/TrendChart.vue'
import TimeRangeSelector from '../components/TimeRangeSelector.vue'
import EventList from '../components/EventList.vue'
import PageHeader from '../components/PageHeader.vue'
import LoadingState from '../components/LoadingState.vue'
import ErrorState from '../components/ErrorState.vue'
import EmptyState from '../components/EmptyState.vue'
import { formatBytes } from '../lib/format'

const router = useRouter()
const { window: timeWindow } = usePrometheusTime('168h')

const summary = useApi<SummaryV2>(() => api<SummaryV2>('/api/summary'))
const trend = useApi<TrendData>(() =>
  api<TrendData>(
    `/api/summary/trend?window=${timeWindow.value}&step=${getTimeRangeStep(timeWindow.value)}`,
  ),
)
const activity = useApi<{ items: ActivityItem[] }>(() =>
  api<{ items: ActivityItem[] }>('/api/summary/activity?limit=20'),
)
const workloads = useApi<WorkloadListData>(() =>
  api<WorkloadListData>('/api/workloads?pageSize=200'),
)

async function loadAll() {
  await Promise.all([summary.run(), trend.run(), activity.run(), workloads.run()])
}

const coordinatedCount = computed(() => {
  const items = (workloads.data.value?.items ?? []) as WorkloadItemV2[]
  return items.filter((w) => w.coordinationFactors?.enabled).length
})

const { enabled: autoRefresh, toggle: toggleAutoRefresh } = useAutoRefresh(loadAll)

onMounted(loadAll)

watch(timeWindow, () => {
  trend.run()
})

function gotoFiltered(state: string) {
  router.push(`/workloads?risk=${state}`)
}
function selectAttention(row: AttentionRow) {
  router.push(`/workloads/${row.namespace}/${row.kind}/${row.name}`)
}

const accentColor = 'rgb(124, 58, 237)'
const dimColor = 'rgb(156, 163, 175)'
const successColor = 'rgb(63, 185, 80)'

function cpuTrendSeries() {
  const t = trend.data.value
  if (!t) return []
  return [
    { label: 'Original request', color: dimColor, points: t.cpu.originalRequest },
    { label: 'Current request', color: accentColor, points: t.cpu.request },
    { label: 'Usage', color: successColor, points: t.cpu.usage },
  ]
}

function memTrendSeries() {
  const t = trend.data.value
  if (!t) return []
  return [
    { label: 'Original request', color: dimColor, points: t.memory.originalRequest },
    { label: 'Current request', color: accentColor, points: t.memory.request },
    { label: 'Usage', color: successColor, points: t.memory.usage },
  ]
}
</script>

<template>
  <LoadingState
    v-if="summary.loading.value && !summary.data.value"
    variant="kpi"
    message="Loading overview…"
  />
  <ErrorState v-else-if="summary.error.value" :message="summary.error.value" @retry="loadAll" />
  <template v-else-if="summary.data.value">
    <PageHeader title="Overview" subtitle="Cluster-wide right-sizing impact and attention queue">
      <template #actions>
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

    <!-- Band 1: KPI strip -->
    <div class="stats-row">
      <KpiCard
        label="CPU saved"
        :value="summary.data.value.kpi.cpuSavedCores.toFixed(2) + ' c'"
        :detail="(summary.data.value.kpi.cpuSavedRatio * 100).toFixed(0) + '% of cluster'"
        tone="positive"
        :spark-points="summary.data.value.kpi.cpuSpark7d"
        spark-color="#3fb950"
      />
      <KpiCard
        label="Memory saved"
        :value="formatBytes(summary.data.value.kpi.memSavedBytes)"
        :detail="(summary.data.value.kpi.memSavedRatio * 100).toFixed(0) + '% of cluster'"
        tone="positive"
        :spark-points="summary.data.value.kpi.memSpark7d"
        spark-color="#3fb950"
      />
      <KpiCard
        label="At risk"
        :value="String(summary.data.value.kpi.atRiskCount)"
        tone="danger"
        detail="OOM / blocked"
        @click="gotoFiltered('at-risk')"
        style="cursor: pointer"
      />
      <KpiCard
        label="Drifted"
        :value="String(summary.data.value.kpi.driftedCount)"
        tone="warn"
        detail=">10% from rec"
        @click="gotoFiltered('drifted')"
        style="cursor: pointer"
      />
      <KpiCard
        label="Coordinated"
        :value="String(coordinatedCount)"
        tone="neutral"
        detail="Autoscaler-aware"
      />
    </div>

    <!-- Band 2: Savings (CPU + Memory side by side) -->
    <div class="card">
      <div class="card-header">
        <h2>Savings</h2>
        <TimeRangeSelector v-model="timeWindow" />
      </div>
      <div class="chart-grid">
        <div>
          <div class="section-label">CPU</div>
          <TrendChart :series="cpuTrendSeries()" unit=" cores" :height="240" />
        </div>
        <div>
          <div class="section-label">Memory</div>
          <TrendChart :series="memTrendSeries()" unit="" :height="240" :y-format="formatBytes" />
        </div>
      </div>
    </div>

    <!-- Band 3: Headroom -->
    <div class="card">
      <div class="card-header"><h2>Cluster headroom</h2></div>
      <div class="chart-grid">
        <HeadroomBar
          label="CPU"
          :used="summary.data.value.headroom.cpu.used"
          :idle="summary.data.value.headroom.cpu.idle"
          :free="summary.data.value.headroom.cpu.free"
        />
        <HeadroomBar
          label="Memory"
          :used="summary.data.value.headroom.memory.used"
          :idle="summary.data.value.headroom.memory.idle"
          :free="summary.data.value.headroom.memory.free"
        />
      </div>
    </div>

    <!-- Band 4: Attention -->
    <div class="card">
      <div class="card-header"><h2>Needs attention</h2></div>
      <AttentionQueue :groups="summary.data.value.attention" @select="selectAttention" />
    </div>

    <!-- Band 5: Policy effectiveness -->
    <div class="card">
      <div class="card-header"><h2>Policy effectiveness</h2></div>
      <EmptyState
        v-if="summary.data.value.policies.length === 0"
        compact
        message="No policies have produced savings data yet."
      />
      <div v-else class="table-wrap">
        <table class="responsive">
          <thead>
            <tr>
              <th>Policy</th>
              <th>Workloads</th>
              <th>CPU saved</th>
              <th>Mem saved</th>
              <th>At risk</th>
            </tr>
          </thead>
          <tbody>
            <tr
              v-for="p in summary.data.value.policies"
              :key="p.name"
              @click="router.push(`/policies/${p.name}`)"
            >
              <td data-label="Policy" class="font-semibold">{{ p.name }}</td>
              <td data-label="Workloads">{{ p.workloadCount }}</td>
              <td data-label="CPU saved">
                <code>{{ p.cpuSavingsCores.toFixed(2) }}c</code>
              </td>
              <td data-label="Mem saved">
                <code>{{ formatBytes(p.memSavingsBytes) }}</code>
              </td>
              <td data-label="At risk">
                <span v-if="p.atRiskCount > 0" class="badge badge-red">{{ p.atRiskCount }}</span
                ><span v-else>-</span>
              </td>
            </tr>
          </tbody>
        </table>
      </div>
    </div>

    <!-- Band 6: Activity -->
    <div class="card">
      <div class="card-header"><h2>Recent activity</h2></div>
      <EmptyState
        v-if="!activity.data.value?.items?.length"
        compact
        message="No recent activity in this cluster yet."
      />
      <EventList v-else :items="activity.data.value.items" />
    </div>
  </template>
</template>
