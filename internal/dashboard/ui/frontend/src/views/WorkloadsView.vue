<script setup lang="ts">
import { ref, watch, computed, onMounted } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { api, type WorkloadListData, type CoordinationFactors } from '../lib/api'
import { useApi } from '../composables/useApi'
import { useAutoRefresh } from '../composables/useAutoRefresh'
import { useSorting } from '../composables/useSorting'
import RiskBadge from '../components/RiskBadge.vue'
import { timeAgo } from '../lib/format'

const route = useRoute()
const router = useRouter()

const nsFilter = ref('')
const kindFilter = ref('')
const automatedFilter = ref('')
const riskFilter = ref(String(route.query.risk || ''))
const autoscalerFilter = ref(String(route.query.autoscaler || ''))
const search = ref('')
const page = ref(1)
let searchTimer: ReturnType<typeof setTimeout> | null = null

const { sort, sortArrow, applySorting } = useSorting('allWorkloads')

function buildQs() {
  const qs = new URLSearchParams({ page: String(page.value), pageSize: '50' })
  if (nsFilter.value) qs.set('namespace', nsFilter.value)
  if (kindFilter.value) qs.set('kind', kindFilter.value)
  if (automatedFilter.value) qs.set('automated', automatedFilter.value)
  if (riskFilter.value) qs.set('risk', riskFilter.value)
  if (autoscalerFilter.value) qs.set('autoscaler', autoscalerFilter.value)
  if (search.value) qs.set('search', search.value)
  return qs
}

const list = useApi<WorkloadListData>(() =>
  api<WorkloadListData>('/api/workloads?' + buildQs().toString()),
)

function load() {
  list.run()
}

const { enabled: autoRefresh, toggle: toggleAutoRefresh } = useAutoRefresh(load)

onMounted(load)
watch([nsFilter, kindFilter, automatedFilter, page], load)

watch([riskFilter, autoscalerFilter], () => {
  router.replace({
    query: {
      ...route.query,
      risk: riskFilter.value || undefined,
      autoscaler: autoscalerFilter.value || undefined,
    },
  })
  page.value = 1
  load()
})

function onSearch(val: string) {
  if (searchTimer) clearTimeout(searchTimer)
  searchTimer = setTimeout(() => {
    search.value = val
    page.value = 1
    load()
  }, 300)
}

const sorted = computed(() => applySorting(list.data.value?.items || []))
const totalPages = computed(() => {
  if (!list.data.value) return 1
  return Math.ceil(list.data.value.total / (list.data.value.pageSize || 50))
})

function isMeaningful(v: number | undefined): v is number {
  return typeof v === 'number' && Math.abs(v - 1) > 1e-6
}

function hasCoordinationFactors(cf?: CoordinationFactors): boolean {
  if (!cf?.enabled) return false
  return (
    isMeaningful(cf.cpuOverhead) || isMeaningful(cf.memoryOverhead) || isMeaningful(cf.cpuReplica)
  )
}
</script>

<template>
  <div v-if="list.loading.value && !list.data.value" class="loading">
    <div class="spinner"></div>
    Loading workloads...
  </div>
  <div v-else-if="list.error.value" class="card">
    <p style="color: var(--red)">Error: {{ list.error.value }}</p>
  </div>
  <template v-else-if="list.data.value">
    <div
      class="page-header"
      style="display: flex; align-items: flex-start; justify-content: space-between"
    >
      <div>
        <h1>Workloads</h1>
        <p>All workloads across the cluster</p>
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
        <div class="stat-label">Total</div>
        <div class="stat-value">{{ list.data.value.counts.total }}</div>
      </div>
      <div class="stat-card">
        <div class="stat-label">Automated</div>
        <div class="stat-value" style="color: var(--green)">
          {{ list.data.value.counts.automated }}
        </div>
      </div>
      <div class="stat-card">
        <div class="stat-label">Manual</div>
        <div class="stat-value" style="color: var(--text-dim)">
          {{ list.data.value.counts.manual }}
        </div>
      </div>
    </div>

    <div class="card">
      <div class="card-header">
        <h2>Workloads</h2>
        <div class="filter-bar">
          <select v-model="nsFilter" @change="page = 1">
            <option value="">All namespaces</option>
            <option v-for="ns in (list.data.value.namespaces || []).sort()" :key="ns" :value="ns">
              {{ ns }}
            </option>
          </select>
          <select v-model="kindFilter" @change="page = 1">
            <option value="">All kinds</option>
            <option v-for="k in (list.data.value.kinds || []).sort()" :key="k" :value="k">
              {{ k }}
            </option>
          </select>
          <select v-model="automatedFilter" @change="page = 1">
            <option value="">All status</option>
            <option value="true">Automated</option>
            <option value="false">Manual</option>
          </select>
          <select v-model="riskFilter">
            <option value="">Any risk</option>
            <option value="safe">Safe</option>
            <option value="drifted">Drifted</option>
            <option value="at-risk">At risk</option>
            <option value="blocked">Blocked</option>
          </select>
          <select v-model="autoscalerFilter">
            <option value="">Any autoscaler</option>
            <option value="has-autoscaler">Has autoscaler</option>
            <option value="no-autoscaler">No autoscaler</option>
          </select>
          <input
            type="text"
            placeholder="Search by name..."
            :value="search"
            @input="onSearch(($event.target as HTMLInputElement).value)"
          />
        </div>
      </div>

      <div v-if="sorted.length === 0" class="empty-state">
        <p>No workloads found matching the filters.</p>
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
                <th class="sort-header" @click="sort('policyName')">
                  Policy<span v-html="sortArrow('policyName')"></span>
                </th>
                <th>Containers</th>
                <th class="sort-header" @click="sort('lastRecycledAt')">
                  Last recycled<span v-html="sortArrow('lastRecycledAt')"></span>
                </th>
              </tr>
            </thead>
            <tbody>
              <tr
                v-for="w in sorted"
                :key="w.namespace + '/' + w.kind + '/' + w.name"
                @click="router.push(`/workloads/${w.namespace}/${w.kind}/${w.name}`)"
              >
                <td style="color: var(--text-dim)">{{ w.namespace }}</td>
                <td>
                  <span class="kind-badge" :class="'kind-' + w.kind">{{ w.kind }}</span>
                </td>
                <td style="font-weight: 600">
                  {{ w.name
                  }}<span
                    v-if="w.autoscalerPresent"
                    class="badge badge-blue"
                    style="margin-left: 8px"
                    >Autoscaler</span
                  ><span
                    v-if="w.coordinationFactors?.enabled"
                    class="badge badge-blue"
                    style="margin-left: 8px"
                    >Coordinated<template v-if="hasCoordinationFactors(w.coordinationFactors)">
                      <span v-if="isMeaningful(w.coordinationFactors.cpuOverhead)">
                        &times;{{ w.coordinationFactors.cpuOverhead!.toFixed(2) }} CPU</span
                      ><span v-if="isMeaningful(w.coordinationFactors.memoryOverhead)">
                        &times;{{ w.coordinationFactors.memoryOverhead!.toFixed(2) }} mem</span
                      ><span v-if="isMeaningful(w.coordinationFactors.cpuReplica)">
                        &middot; replica &times;{{
                          w.coordinationFactors.cpuReplica!.toFixed(2)
                        }}</span
                      >
                    </template></span
                  >
                </td>
                <td><RiskBadge :state="w.riskState" /></td>
                <td>
                  <code v-if="w.driftPercent">{{ w.driftPercent.toFixed(1) }}%</code
                  ><span v-else style="color: var(--text-dim)">-</span>
                </td>
                <td>
                  <a
                    v-if="w.policyName"
                    href="#"
                    @click.stop.prevent="router.push(`/policies/${w.policyName}`)"
                    >{{ w.policyName }}</a
                  ><span v-else>-</span>
                </td>
                <td style="color: var(--text-dim)">{{ w.containers.length }}</td>
                <td style="color: var(--text-dim)">
                  {{ w.lastRecycledAt ? timeAgo(w.lastRecycledAt) : '-' }}
                </td>
              </tr>
            </tbody>
          </table>
        </div>
        <div v-if="totalPages > 1" class="pagination">
          <button :disabled="page <= 1" @click="page--">Previous</button>
          <span>Page {{ page }} of {{ totalPages }}</span>
          <button :disabled="page >= totalPages" @click="page++">Next</button>
        </div>
      </template>
    </div>
  </template>
</template>
