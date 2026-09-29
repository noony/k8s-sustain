<script setup lang="ts">
import { watch, computed, onMounted } from 'vue'
import { api, type WorkloadListData, type CoordinationFactors } from '../lib/api'
import { useApi } from '../composables/useApi'
import { useAutoRefresh } from '../composables/useAutoRefresh'
import { useRowLink } from '../composables/useRowLink'
import { useListQuery } from '../composables/useListQuery'
import { workloadPath } from '../lib/routes'
import RiskBadge from '../components/RiskBadge.vue'
import PageHeader from '../components/PageHeader.vue'
import LoadingState from '../components/LoadingState.vue'
import ErrorState from '../components/ErrorState.vue'
import EmptyState from '../components/EmptyState.vue'
import Combobox from '../components/Combobox.vue'
import { timeAgo } from '../lib/format'

const { openRow } = useRowLink()
const {
  filterRef,
  page,
  sort,
  sortArrow,
  searchInput,
  onSearch,
  hasFilters,
  clearFilters,
  apiQuery,
  onQueryChange,
  totalPagesOf,
  clampPage,
} = useListQuery({
  filterKeys: ['namespace', 'kind', 'automated', 'active', 'risk', 'autoscaler', 'search'],
  defaultSort: 'name',
})

const nsFilter = filterRef('namespace')
const kindFilter = filterRef('kind')
const automatedFilter = filterRef('automated')
const activeFilter = filterRef('active')
const riskFilter = filterRef('risk')
const autoscalerFilter = filterRef('autoscaler')

const list = useApi<WorkloadListData>(() =>
  api<WorkloadListData>('/api/workloads?' + apiQuery.value),
)

function load() {
  list.run()
}

useAutoRefresh(load)
onMounted(load)
onQueryChange(load)

const sorted = computed(() => list.data.value?.items || [])
const totalPages = computed(() => totalPagesOf(list.data.value))
watch(() => list.data.value, clampPage)

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
  <LoadingState
    v-if="list.loading.value && !list.data.value"
    variant="kpi"
    message="Loading workloads…"
  />
  <ErrorState v-else-if="list.error.value" :message="list.error.value" @retry="load" />
  <template v-else-if="list.data.value">
    <PageHeader title="Workloads" subtitle="All workloads across the cluster"> </PageHeader>

    <div class="stats-row">
      <div class="stat-card">
        <div class="stat-label">Total</div>
        <div class="stat-value">{{ list.data.value.counts.total }}</div>
      </div>
      <div class="stat-card">
        <div class="stat-label">Automated</div>
        <div class="stat-value text-success">{{ list.data.value.counts.automated }}</div>
      </div>
      <div class="stat-card">
        <div class="stat-label">Manual</div>
        <div class="stat-value text-dim">{{ list.data.value.counts.manual }}</div>
      </div>
    </div>

    <div class="card">
      <div class="card-header">
        <h2>Workloads</h2>
        <div class="filter-bar">
          <Combobox
            v-model="nsFilter"
            :options="list.data.value.namespaces || []"
            placeholder="Namespace…"
            all-label="All namespaces"
          />
          <Combobox
            v-model="kindFilter"
            :options="list.data.value.kinds || []"
            placeholder="Kind…"
            all-label="All kinds"
            min-width="140px"
          />
          <select v-model="automatedFilter">
            <option value="">All status</option>
            <option value="true">Automated</option>
            <option value="false">Manual</option>
          </select>
          <select v-model="activeFilter">
            <option value="">Any lifecycle</option>
            <option value="true">Active</option>
            <option value="false">Inactive</option>
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
            :value="searchInput"
            @input="onSearch(($event.target as HTMLInputElement).value)"
          />
          <button
            class="btn btn-secondary btn-sm"
            type="button"
            data-test="reset-filters"
            :disabled="!hasFilters"
            @click="clearFilters"
          >
            Reset filters
          </button>
        </div>
      </div>

      <EmptyState
        v-if="sorted.length === 0"
        :icon="hasFilters ? 'search' : 'workload'"
        :title="hasFilters ? 'No matches' : 'No workloads yet'"
        :message="
          hasFilters
            ? 'No workloads match the current filters. Try widening your search.'
            : 'Workloads will appear here once discovered in this cluster.'
        "
      >
        <template v-if="hasFilters" #actions>
          <button class="btn btn-secondary btn-sm" type="button" @click="clearFilters">
            Clear filters
          </button>
        </template>
      </EmptyState>
      <template v-else>
        <div class="table-wrap">
          <table class="responsive">
            <thead>
              <tr>
                <th class="sort-header" @click="sort('namespace')">
                  Namespace<span>{{ sortArrow('namespace') }}</span>
                </th>
                <th class="sort-header" @click="sort('kind')">
                  Kind<span>{{ sortArrow('kind') }}</span>
                </th>
                <th class="sort-header" @click="sort('name')">
                  Name<span>{{ sortArrow('name') }}</span>
                </th>
                <th>Risk</th>
                <th class="sort-header" @click="sort('stalePods')">
                  Drift<span>{{ sortArrow('stalePods') }}</span>
                </th>
                <th class="sort-header" @click="sort('policyName')">
                  Policy<span>{{ sortArrow('policyName') }}</span>
                </th>
                <th>Containers</th>
              </tr>
            </thead>
            <tbody>
              <tr
                v-for="w in sorted"
                :key="w.namespace + '/' + w.kind + '/' + w.name"
                @click="openRow(workloadPath(w), $event)"
                @auxclick="openRow(workloadPath(w), $event)"
              >
                <td data-label="Namespace" class="text-dim">{{ w.namespace }}</td>
                <td data-label="Kind">
                  <span class="kind-badge" :class="'kind-' + w.kind">{{ w.kind }}</span>
                </td>
                <td data-label="Name" class="font-semibold">
                  <RouterLink :to="workloadPath(w)" class="row-link" @click.stop @auxclick.stop>{{
                    w.name
                  }}</RouterLink>
                  <span
                    v-if="w.active === false"
                    class="badge badge-dim gap-2"
                    :title="w.lastSeenAt"
                    >Inactive<template v-if="w.lastSeenAt">
                      · last seen {{ timeAgo(w.lastSeenAt) }}</template
                    ></span
                  >
                  <span v-if="w.autoscalerPresent" class="badge badge-blue gap-2">Autoscaler</span>
                  <span v-if="w.coordinationFactors?.enabled" class="badge badge-blue gap-2"
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
                <td data-label="Risk"><RiskBadge :state="w.riskState" /></td>
                <td data-label="Drift">
                  <code v-if="w.stalePods">{{
                    w.totalPods ? `${w.stalePods}/${w.totalPods}` : w.stalePods
                  }}</code>
                  <span v-else class="text-dim">-</span>
                </td>
                <td data-label="Policy">
                  <RouterLink
                    v-if="w.policyName"
                    :to="`/policies/${w.policyName}`"
                    @click.stop
                    @auxclick.stop
                    >{{ w.policyName }}</RouterLink
                  ><span v-else>-</span>
                </td>
                <td data-label="Containers" class="text-dim">{{ w.containers.length }}</td>
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
