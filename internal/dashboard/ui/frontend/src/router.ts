import { createRouter, createWebHashHistory } from 'vue-router'
import OverviewView from './views/OverviewView.vue'
import PoliciesView from './views/PoliciesView.vue'
import PolicyDetailView from './views/PolicyDetailView.vue'
import WorkloadsView from './views/WorkloadsView.vue'
import WorkloadDetailView from './views/WorkloadDetailView.vue'
import SimulatorView from './views/SimulatorView.vue'

const routes = [
  { path: '/', redirect: '/overview' },
  { path: '/overview', component: OverviewView },
  { path: '/policies', component: PoliciesView },
  { path: '/policies/:name', component: PolicyDetailView, props: true },
  { path: '/workloads', component: WorkloadsView },
  { path: '/workloads/:namespace/:kind/:name', component: WorkloadDetailView, props: true },
  { path: '/simulator', component: SimulatorView },
  { path: '/simulator/:namespace/:kind/:name', component: SimulatorView, props: true },
]

export const router = createRouter({
  history: createWebHashHistory(),
  routes,
})
