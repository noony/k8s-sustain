<script setup lang="ts">
import { ref, onMounted, watch, computed } from 'vue'
import { useRouter } from 'vue-router'
import { api, type WorkloadListData } from '../lib/api'
import { useAutoRefresh } from '../composables/useAutoRefresh'
import { useSorting } from '../composables/useSorting'

const router = useRouter()
const loading = ref(true)
const error = ref('')
const data = ref<WorkloadListData | null>(null)

const nsFilter = ref('')
const kindFilter = ref('')
const automatedFilter = ref('')
const search = ref('')
const page = ref(1)
let searchTimer: ReturnType<typeof setTimeout> | null = null

const { sort, sortArrow, applySorting } = useSorting('allWorkloads')

async function load() {
  try {
    const qs = new URLSearchParams({ page: String(page.value), pageSize: '50' })
    if (nsFilter.value) qs.set('namespace', nsFilter.value)
    if (kindFilter.value) qs.set('kind', kindFilter.value)
    if (automatedFilter.value) qs.set('automated', automatedFilter.value)
    if (search.value) qs.set('search', search.value)
    data.value = await api<WorkloadListData>('/api/workloads?' + qs)
    error.value = ''
  } catch (e: any) {
    error.value = e.message
  } finally {
    loading.value = false
  }
}

const { enabled: autoRefresh, toggle: toggleAutoRefresh } = useAutoRefresh(load)

onMounted(load)
watch([nsFilter, kindFilter, automatedFilter, page], load)

function onSearch(val: string) {
  if (searchTimer) clearTimeout(searchTimer)
  searchTimer = setTimeout(() => {
    search.value = val
    page.value = 1
    load()
  }, 300)
}

const sorted = computed(() => applySorting(data.value?.items || []))
const totalPages = computed(() => {
  if (!data.value) return 1
  return Math.ceil(data.value.total / (data.value.pageSize || 50))
})
</script>

<template>
  <div v-if="loading" class="loading">
    <div class="spinner"></div>
    Loading workloads...
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
        <div class="stat-value">{{ data.counts.total }}</div>
      </div>
      <div class="stat-card">
        <div class="stat-label">Automated</div>
        <div class="stat-value" style="color: var(--green)">{{ data.counts.automated }}</div>
      </div>
      <div class="stat-card">
        <div class="stat-label">Manual</div>
        <div class="stat-value" style="color: var(--text-dim)">{{ data.counts.manual }}</div>
      </div>
    </div>

    <div class="card">
      <div class="card-header">
        <h2>Workloads</h2>
        <div class="filter-bar">
          <select v-model="nsFilter" @change="page = 1">
            <option value="">All namespaces</option>
            <option v-for="ns in (data.namespaces || []).sort()" :key="ns" :value="ns">
              {{ ns }}
            </option>
          </select>
          <select v-model="kindFilter" @change="page = 1">
            <option value="">All kinds</option>
            <option v-for="k in (data.kinds || []).sort()" :key="k" :value="k">{{ k }}</option>
          </select>
          <select v-model="automatedFilter" @change="page = 1">
            <option value="">All status</option>
            <option value="true">Automated</option>
            <option value="false">Manual</option>
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
                <th>Status</th>
                <th class="sort-header" @click="sort('policyName')">
                  Policy<span v-html="sortArrow('policyName')"></span>
                </th>
                <th>Containers</th>
                <th>CPU Req</th>
                <th>Mem Req</th>
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
                <td style="font-weight: 600">{{ w.name }}</td>
                <td>
                  <span v-if="w.automated" class="badge badge-green">Automated</span>
                  <span v-else class="badge badge-dim">Manual</span>
                </td>
                <td>
                  <a
                    v-if="w.policyName"
                    href="#"
                    @click.stop.prevent="router.push(`/policies/${w.policyName}`)"
                    >{{ w.policyName }}</a
                  >
                  <span v-else>-</span>
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
        <div v-if="totalPages > 1" class="pagination">
          <button :disabled="page <= 1" @click="page--">Previous</button>
          <span>Page {{ page }} of {{ totalPages }}</span>
          <button :disabled="page >= totalPages" @click="page++">Next</button>
        </div>
      </template>
    </div>
  </template>
</template>
