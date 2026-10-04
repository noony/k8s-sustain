import { mount, flushPromises } from '@vue/test-utils'
import { describe, it, expect, vi } from 'vitest'
import { createRouter, createMemoryHistory } from 'vue-router'
import WorkloadDetailView from './WorkloadDetailView.vue'
import * as api from '../lib/api'
vi.mock('../lib/api', async (o) => ({ ...((await o()) as object), api: vi.fn() }))

const router = createRouter({
  history: createMemoryHistory(),
  routes: [{ path: '/', component: WorkloadDetailView }],
})

describe('WorkloadDetailView', () => {
  it('renders status snapshot', async () => {
    ;(api.api as any).mockImplementation((path: string) => {
      if (path.endsWith('/metrics?window=1w&step=20m'))
        return Promise.resolve({ cpu: {}, memory: {} })
      if (path.endsWith('/recommendations?window=1w&step=20m'))
        return Promise.resolve({ automated: false })
      if (path.match(/\/api\/workloads\/[^/]+\/[^/]+\/[^/]+$/))
        return Promise.resolve({
          updateMode: 'Ongoing',
          riskState: 'at-risk',
          oom24h: 2,
          stalePods: 1,
          totalPods: 4,
          recentEvents: [],
        })
      return Promise.resolve({})
    })
    const w = mount(WorkloadDetailView, {
      props: { namespace: 'a', kind: 'Deployment', name: 'web' },
      global: {
        plugins: [router],
        stubs: ['TimeRangePicker', 'TrendChart'],
      },
    })
    await flushPromises()
    expect(w.text()).toContain('Ongoing')
    expect(w.text()).toContain('OOM')
    expect(w.text()).toContain('1/4 pods')
    expect(w.findComponent({ name: 'RiskBadge' }).props('state')).toBe('at-risk')
  })

  it('renders the risk state the backend classified, not one derived from the signals', async () => {
    ;(api.api as any).mockImplementation((path: string) => {
      if (path.endsWith('/metrics?window=1w&step=20m'))
        return Promise.resolve({ cpu: {}, memory: {} })
      if (path.endsWith('/recommendations?window=1w&step=20m'))
        return Promise.resolve({ automated: false })
      if (path.match(/\/api\/workloads\/[^/]+\/[^/]+\/[^/]+$/))
        return Promise.resolve({
          riskState: 'blocked',
          oom24h: 3,
          stalePods: 0,
          totalPods: 2,
          blocked: { reason: 'patch', attempts: 2 },
          recentEvents: [],
        })
      return Promise.resolve({})
    })
    const w = mount(WorkloadDetailView, {
      props: { namespace: 'a', kind: 'Deployment', name: 'web' },
      global: {
        plugins: [router],
        stubs: ['TimeRangePicker', 'TrendChart'],
      },
    })
    await flushPromises()
    expect(w.findComponent({ name: 'RiskBadge' }).text()).toBe('Blocked')
  })
})
