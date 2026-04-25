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
  it('renders status snapshot and HPA badge', async () => {
    ;(api.api as any).mockImplementation((path: string) => {
      if (path.endsWith('/metrics?window=168h&step=15m'))
        return Promise.resolve({ cpu: {}, memory: {} })
      if (path.endsWith('/recommendations?window=168h&step=15m'))
        return Promise.resolve({ automated: false })
      if (path.match(/\/api\/workloads\/[^/]+\/[^/]+\/[^/]+$/))
        return Promise.resolve({
          updateMode: 'Ongoing',
          oom24h: 2,
          driftPercent: 18,
          hpaMode: 'HpaAware',
          recentEvents: [],
        })
      return Promise.resolve({})
    })
    const w = mount(WorkloadDetailView, {
      props: { namespace: 'a', kind: 'Deployment', name: 'web' },
      global: {
        plugins: [router],
        stubs: ['TimeRangeSelector', 'TrendChart'],
      },
    })
    await flushPromises()
    expect(w.text()).toContain('HPA')
    expect(w.text()).toContain('Ongoing')
    expect(w.text()).toContain('OOM')
  })
})
