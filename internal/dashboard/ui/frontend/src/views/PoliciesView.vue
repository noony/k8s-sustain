<script setup lang="ts">
import { ref, onMounted, computed } from 'vue'
import { useRouter } from 'vue-router'
import { api, type PolicySummary } from '../lib/api'
import { timeAgo } from '../lib/format'
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

const readyCount = computed(
  () =>
    policies.value.filter(
      (p) => p.conditions && p.conditions.some((c) => c.type === 'Ready' && c.status === 'True'),
    ).length,
)

const nsCount = computed(() => {
  const ns = new Set(policies.value.flatMap((p) => p.namespaces || []))
  return ns.size || 'All'
})

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
          <div class="stat-label">Total Policies</div>
          <div class="stat-value">{{ policies.length }}</div>
        </div>
        <div class="stat-card">
          <div class="stat-label">Ready</div>
          <div class="stat-value" style="color: var(--green)">{{ readyCount }}</div>
        </div>
        <div class="stat-card">
          <div class="stat-label">Namespaces Covered</div>
          <div class="stat-value">{{ nsCount }}</div>
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
                <th>Namespaces</th>
                <th>Workload Types</th>
                <th class="sort-header" @click="sort('createdAt')">
                  Created<span v-html="sortArrow('createdAt')"></span>
                </th>
              </tr>
            </thead>
            <tbody>
              <tr v-for="p in sorted" :key="p.name" @click="router.push(`/policies/${p.name}`)">
                <td style="font-weight: 600">{{ p.name }}</td>
                <td><StatusBadge :conditions="p.conditions" /></td>
                <td>
                  {{ p.namespaces?.length ? p.namespaces.join(', ') : ''
                  }}<span v-if="!p.namespaces?.length" class="badge badge-dim">All</span>
                </td>
                <td>{{ updateTypeBadges(p.update) }}</td>
                <td style="color: var(--text-dim)">{{ timeAgo(p.createdAt) }}</td>
              </tr>
            </tbody>
          </table>
        </div>
      </div>
    </template>
  </template>
</template>
