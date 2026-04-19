<script setup lang="ts">
import { ref, onMounted } from 'vue'
import { useRouter } from 'vue-router'
import { api, type OverviewData, type OverviewWorkload } from '../lib/api'
import { useAutoRefresh } from '../composables/useAutoRefresh'
import { useSorting } from '../composables/useSorting'

const router = useRouter()
const loading = ref(true)
const error = ref('')
const data = ref<OverviewData | null>(null)

const { sort, sortArrow, applySorting } = useSorting('overview')

async function load() {
  try {
    data.value = await api<OverviewData>('/api/summary')
    error.value = ''
  } catch (e: any) {
    error.value = e.message
  } finally {
    loading.value = false
  }
}

const { enabled: autoRefresh, toggle: toggleAutoRefresh } = useAutoRefresh(load)

onMounted(load)

function attentionWorkloads(): OverviewWorkload[] {
  if (!data.value?.workloads) return []
  const filtered = data.value.workloads.filter(
    (w) => Math.abs(w.cpuDeltaPercent) > 5 || Math.abs(w.memDeltaPercent) > 5,
  )
  filtered.sort((a, b) => {
    const da = Math.max(Math.abs(a.cpuDeltaPercent), Math.abs(a.memDeltaPercent))
    const db = Math.max(Math.abs(b.cpuDeltaPercent), Math.abs(b.memDeltaPercent))
    return db - da
  })
  return applySorting(filtered)
}

function deltaClass(pct: number): string {
  if (pct === 0) return ''
  return pct < -5 ? 'saving' : pct > 5 ? 'increase' : 'neutral'
}

function savingsClass(millis: number): string {
  return millis > 0 ? 'savings-positive' : millis < 0 ? 'savings-negative' : ''
}
</script>

<template>
  <div v-if="loading" class="loading">
    <div class="spinner"></div>
    Loading overview...
  </div>
  <div v-else-if="error" class="card">
    <p style="color: var(--red)">Error: {{ error }}</p>
  </div>
  <template v-else-if="data">
    <div
      class="page-header"
      style="display: flex; align-items: flex-start; justify-content: space-between"
    >
      <div>
        <h1>Overview</h1>
        <p>Cluster-wide resource right-sizing summary</p>
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

    <div class="stats-row">
      <div class="stat-card">
        <div class="stat-label">Total Workloads</div>
        <div class="stat-value">{{ data.totalWorkloads }}</div>
      </div>
      <div class="stat-card">
        <div class="stat-label">Automated</div>
        <div class="stat-value" style="color: var(--green)">{{ data.automated }}</div>
        <div class="stat-detail">{{ data.manual }} manual</div>
      </div>
      <div class="stat-card">
        <div class="stat-label">CPU Usage vs Recommendation</div>
        <div class="stat-value" :class="savingsClass(data.cpu.savingsMillis)">
          {{ data.cpu.savingsFormatted }}
        </div>
        <div class="stat-detail">
          {{ data.cpu.currentFormatted }} &rarr; {{ data.cpu.recommendedFormatted
          }}<template v-if="data.cpu.savingsPercent"> ({{ data.cpu.savingsPercent }}%)</template>
        </div>
      </div>
      <div class="stat-card">
        <div class="stat-label">Memory Usage vs Recommendation</div>
        <div class="stat-value" :class="savingsClass(data.memory.savingsMillis)">
          {{ data.memory.savingsFormatted }}
        </div>
        <div class="stat-detail">
          {{ data.memory.currentFormatted }} &rarr; {{ data.memory.recommendedFormatted
          }}<template v-if="data.memory.savingsPercent">
            ({{ data.memory.savingsPercent }}%)</template
          >
        </div>
      </div>
    </div>

    <div class="card">
      <div class="card-header">
        <h2>Needs Attention</h2>
        <span class="badge badge-yellow">{{ attentionWorkloads().length }} workloads</span>
      </div>
      <div v-if="attentionWorkloads().length === 0" class="empty-state">
        <p>All automated workloads are right-sized. Nice!</p>
      </div>
      <div v-else class="table-wrap">
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
              <th>Policy</th>
              <th class="sort-header" @click="sort('cpuDeltaPercent')">
                CPU Delta<span v-html="sortArrow('cpuDeltaPercent')"></span>
              </th>
              <th class="sort-header" @click="sort('memDeltaPercent')">
                Memory Delta<span v-html="sortArrow('memDeltaPercent')"></span>
              </th>
            </tr>
          </thead>
          <tbody>
            <tr
              v-for="w in attentionWorkloads()"
              :key="w.namespace + '/' + w.name"
              @click="router.push(`/workloads/${w.namespace}/${w.kind}/${w.name}`)"
            >
              <td style="color: var(--text-dim)">{{ w.namespace }}</td>
              <td>
                <span class="kind-badge" :class="'kind-' + w.kind">{{ w.kind }}</span>
              </td>
              <td style="font-weight: 600">{{ w.name }}</td>
              <td>
                <a href="#" @click.stop.prevent="router.push(`/policies/${w.policyName}`)">{{
                  w.policyName
                }}</a>
              </td>
              <td>
                <span
                  v-if="w.cpuDeltaPercent !== 0"
                  class="delta-badge"
                  :class="deltaClass(w.cpuDeltaPercent)"
                >
                  <span v-html="w.cpuDeltaPercent < 0 ? '&#8595;' : '&#8593;'"></span>
                  {{ Math.abs(w.cpuDeltaPercent).toFixed(1) }}%
                </span>
              </td>
              <td>
                <span
                  v-if="w.memDeltaPercent !== 0"
                  class="delta-badge"
                  :class="deltaClass(w.memDeltaPercent)"
                >
                  <span v-html="w.memDeltaPercent < 0 ? '&#8595;' : '&#8593;'"></span>
                  {{ Math.abs(w.memDeltaPercent).toFixed(1) }}%
                </span>
              </td>
            </tr>
          </tbody>
        </table>
      </div>
    </div>
  </template>
</template>
