<script setup lang="ts">
import { ref, onMounted, computed } from 'vue'
import { useRouter } from 'vue-router'
import { api, type PolicySummary } from '../lib/api'
import { timeAgo, formatBytes } from '../lib/format'
import { useAutoRefresh } from '../composables/useAutoRefresh'
import { useSorting } from '../composables/useSorting'
import StatusBadge from '../components/StatusBadge.vue'
import PageHeader from '../components/PageHeader.vue'
import LoadingState from '../components/LoadingState.vue'
import ErrorState from '../components/ErrorState.vue'
import EmptyState from '../components/EmptyState.vue'

const router = useRouter()
const loading = ref(true)
const error = ref('')
const policies = ref<PolicySummary[]>([])

const { sort, sortArrow, applySorting } = useSorting('policies')

async function load() {
  try {
    policies.value = await api<PolicySummary[]>('/api/policies')
    error.value = ''
  } catch (e: any) {
    error.value = e.message
  } finally {
    loading.value = false
  }
}

useAutoRefresh(load)

onMounted(load)

const sorted = computed(() => applySorting(policies.value))

const totalWorkloads = computed(() =>
  policies.value.reduce((a, p) => a + (p.workloadCount || 0), 0),
)
const totalCpu = computed(() => policies.value.reduce((a, p) => a + (p.cpuSavingsCores || 0), 0))
const totalMem = computed(() => policies.value.reduce((a, p) => a + (p.memSavingsBytes || 0), 0))

function updateTypeBadges(update?: Record<string, string>): string {
  if (!update) return ''
  const types: string[] = []
  if (update.deployment) types.push(`Deploy:${update.deployment}`)
  if (update.statefulSet) types.push(`STS:${update.statefulSet}`)
  if (update.daemonSet) types.push(`DS:${update.daemonSet}`)
  if (update.cronJob) types.push(`CJ:${update.cronJob}`)
  return types.join(', ') || '-'
}
</script>

<template>
  <LoadingState v-if="loading" variant="kpi" message="Loading policies…" />
  <ErrorState v-else-if="error" :message="error" @retry="load" />
  <template v-else>
    <PageHeader title="Policies" subtitle="All right-sizing policies in your cluster"> </PageHeader>

    <EmptyState
      v-if="policies.length === 0"
      icon="policy"
      title="No policies yet"
      message="Create a Policy resource to start right-sizing workloads in this cluster."
    />

    <template v-else>
      <div class="stats-row">
        <div class="stat-card">
          <div class="stat-label">Total policies</div>
          <div class="stat-value">{{ policies.length }}</div>
        </div>
        <div class="stat-card">
          <div class="stat-label">Workloads covered</div>
          <div class="stat-value">{{ totalWorkloads }}</div>
        </div>
        <div class="stat-card">
          <div class="stat-label">Cluster CPU saved</div>
          <div class="stat-value text-success">{{ totalCpu.toFixed(2) }}c</div>
        </div>
        <div class="stat-card">
          <div class="stat-label">Cluster Mem saved</div>
          <div class="stat-value text-success">{{ formatBytes(totalMem) }}</div>
        </div>
      </div>

      <div class="card">
        <div class="table-wrap">
          <table class="responsive">
            <thead>
              <tr>
                <th class="sort-header" @click="sort('name')">
                  Name<span v-html="sortArrow('name')"></span>
                </th>
                <th>Status</th>
                <th>Mode</th>
                <th class="sort-header" @click="sort('workloadCount')">
                  Workloads<span v-html="sortArrow('workloadCount')"></span>
                </th>
                <th class="sort-header" @click="sort('cpuSavingsCores')">
                  CPU saved<span v-html="sortArrow('cpuSavingsCores')"></span>
                </th>
                <th class="sort-header" @click="sort('memSavingsBytes')">
                  Mem saved<span v-html="sortArrow('memSavingsBytes')"></span>
                </th>
                <th class="sort-header" @click="sort('atRiskCount')">
                  At risk<span v-html="sortArrow('atRiskCount')"></span>
                </th>
                <th>Last applied</th>
              </tr>
            </thead>
            <tbody>
              <tr v-for="p in sorted" :key="p.name" @click="router.push(`/policies/${p.name}`)">
                <td data-label="Name" class="font-semibold">{{ p.name }}</td>
                <td data-label="Status"><StatusBadge :conditions="p.conditions" /></td>
                <td data-label="Mode">{{ updateTypeBadges(p.update) }}</td>
                <td data-label="Workloads">{{ p.workloadCount || 0 }}</td>
                <td data-label="CPU saved">
                  <code>{{ (p.cpuSavingsCores || 0).toFixed(2) }}c</code>
                </td>
                <td data-label="Mem saved">
                  <code>{{ formatBytes(p.memSavingsBytes || 0) }}</code>
                </td>
                <td data-label="At risk">
                  <span v-if="p.atRiskCount" class="badge badge-red">{{ p.atRiskCount }}</span
                  ><span v-else>-</span>
                </td>
                <td data-label="Last applied" class="text-dim">
                  {{ p.lastAppliedAt ? timeAgo(p.lastAppliedAt) : '-' }}
                </td>
              </tr>
            </tbody>
          </table>
        </div>
      </div>
    </template>
  </template>
</template>
