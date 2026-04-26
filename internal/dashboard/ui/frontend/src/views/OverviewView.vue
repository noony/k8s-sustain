<script setup lang="ts">
import { onMounted, computed } from 'vue'
import { useRouter } from 'vue-router'
import {
  api,
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
import EventList from '../components/EventList.vue'
import { formatBytes } from '../lib/format'

const router = useRouter()
const { window: timeWindow } = usePrometheusTime('168h')

const summary = useApi<SummaryV2>(() => api<SummaryV2>('/api/summary'))
const trend = useApi<TrendData>(() =>
  api<TrendData>(`/api/summary/trend?window=${timeWindow.value}`),
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

function gotoFiltered(state: string) {
  router.push(`/workloads?risk=${state}`)
}
function selectAttention(row: AttentionRow) {
  router.push(`/workloads/${row.namespace}/${row.kind}/${row.name}`)
}

function trendSeries() {
  if (!trend.data.value) return []
  return [
    { label: 'CPU saved', color: '#6366f1', points: trend.data.value.cpu },
    { label: 'Mem saved', color: '#06b6d4', points: trend.data.value.memory },
  ]
}
</script>

<template>
  <div v-if="summary.loading.value && !summary.data.value" class="loading">
    <div class="spinner"></div>
    Loading overview...
  </div>
  <div v-else-if="summary.error.value" class="card">
    <p style="color: var(--red)">Error: {{ summary.error.value }}</p>
  </div>
  <template v-else-if="summary.data.value">
    <div
      class="page-header"
      style="display: flex; align-items: flex-start; justify-content: space-between"
    >
      <div>
        <h1>Overview</h1>
        <p>Cluster-wide right-sizing impact and attention queue</p>
      </div>
      <label class="auto-refresh">
        <input
          type="checkbox"
          :checked="autoRefresh"
          @change="toggleAutoRefresh(($event.target as HTMLInputElement).checked)"
        />
        Auto-refresh (30s)
      </label>
    </div>

    <!-- Band 1: KPI strip -->
    <div class="stats-row">
      <KpiCard
        label="CPU saved"
        :value="summary.data.value.kpi.cpuSavedCores.toFixed(2) + ' c'"
        :detail="(summary.data.value.kpi.cpuSavedRatio * 100).toFixed(0) + '% of cluster'"
        tone="positive"
        :sparkPoints="summary.data.value.kpi.cpuSpark7d"
        sparkColor="#3fb950"
      />
      <KpiCard
        label="Memory saved"
        :value="formatBytes(summary.data.value.kpi.memSavedBytes)"
        :detail="(summary.data.value.kpi.memSavedRatio * 100).toFixed(0) + '% of cluster'"
        tone="positive"
        :sparkPoints="summary.data.value.kpi.memSpark7d"
        sparkColor="#3fb950"
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

    <!-- Band 2: Trend -->
    <div class="card">
      <div class="card-header"><h2>Cluster savings</h2></div>
      <TrendChart :series="trendSeries()" unit="" :height="240" />
    </div>

    <!-- Band 3: Headroom -->
    <div class="card">
      <div class="card-header"><h2>Cluster headroom</h2></div>
      <div style="display: grid; grid-template-columns: 1fr 1fr; gap: 24px">
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
      <div class="table-wrap">
        <table>
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
              <td style="font-weight: 600">{{ p.name }}</td>
              <td>{{ p.workloadCount }}</td>
              <td>
                <code>{{ p.cpuSavingsCores.toFixed(2) }}c</code>
              </td>
              <td>
                <code>{{ formatBytes(p.memSavingsBytes) }}</code>
              </td>
              <td>
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
      <EventList :items="activity.data.value?.items || []" />
    </div>
  </template>
</template>
