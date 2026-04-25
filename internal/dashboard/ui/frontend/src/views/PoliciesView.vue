<script setup lang="ts">
import { ref, onMounted, computed } from 'vue'
import { useRouter } from 'vue-router'
import { api, type PolicySummary } from '../lib/api'
import { timeAgo, formatBytes } from '../lib/format'
import { useAutoRefresh } from '../composables/useAutoRefresh'
import { useSorting } from '../composables/useSorting'
import StatusBadge from '../components/StatusBadge.vue'

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

const { enabled: autoRefresh, toggle: toggleAutoRefresh } = useAutoRefresh(load)

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
  <div v-if="loading" class="loading">
    <div class="spinner"></div>
    Loading policies...
  </div>
  <div v-else-if="error" class="card">
    <p style="color: var(--red)">Error: {{ error }}</p>
  </div>
  <template v-else>
    <div
      class="page-header"
      style="display: flex; align-items: flex-start; justify-content: space-between"
    >
      <div>
        <h1>Policies</h1>
        <p>All right-sizing policies in your cluster</p>
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

    <div v-if="policies.length === 0" class="empty-state">
      <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.5">
        <path
          d="M9 5H7a2 2 0 00-2 2v12a2 2 0 002 2h10a2 2 0 002-2V7a2 2 0 00-2-2h-2M9 5a2 2 0 002 2h2a2 2 0 002-2M9 5a2 2 0 012-2h2a2 2 0 012 2"
        />
      </svg>
      <p>No policies found. Create a Policy resource to get started.</p>
    </div>

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
          <div class="stat-value" style="color: var(--green)">{{ totalCpu.toFixed(2) }}c</div>
        </div>
        <div class="stat-card">
          <div class="stat-label">Cluster Mem saved</div>
          <div class="stat-value" style="color: var(--green)">{{ formatBytes(totalMem) }}</div>
        </div>
      </div>

      <div class="card">
        <div class="table-wrap">
          <table>
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
                <td style="font-weight: 600">{{ p.name }}</td>
                <td><StatusBadge :conditions="p.conditions" /></td>
                <td>{{ updateTypeBadges(p.update) }}</td>
                <td>{{ p.workloadCount || 0 }}</td>
                <td>
                  <code>{{ (p.cpuSavingsCores || 0).toFixed(2) }}c</code>
                </td>
                <td>
                  <code>{{ formatBytes(p.memSavingsBytes || 0) }}</code>
                </td>
                <td>
                  <span v-if="p.atRiskCount" class="badge badge-red">{{ p.atRiskCount }}</span
                  ><span v-else>-</span>
                </td>
                <td style="color: var(--text-dim)">
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
