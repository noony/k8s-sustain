import { mount, flushPromises } from '@vue/test-utils'
import { describe, it, expect, vi } from 'vitest'
import PolicyDetailView from './PolicyDetailView.vue'
import * as api from '../lib/api'
import { createRouter, createMemoryHistory } from 'vue-router'

vi.mock('../lib/api', async (o) => ({ ...((await o()) as object), api: vi.fn() }))

describe('PolicyDetailView', () => {
  it('renders effectiveness chart band and view-as-yaml button', async () => {
    ;(api.api as any).mockImplementation((path: string) => {
      if (path === '/api/policies/p')
        return Promise.resolve({
          name: 'p',
          conditions: [],
          spec: { rightSizing: { resourcesConfigs: { cpu: {}, memory: {} } } },
          effectivenessSeries: { cpu: [], memory: [] },
        })
      if (path.startsWith('/api/policies/p/workloads'))
        return Promise.resolve({ items: [], total: 0, pageSize: 50 })
      return Promise.resolve({})
    })
    const router = createRouter({
      history: createMemoryHistory(),
      routes: [{ path: '/:catchAll(.*)', component: { template: '<div/>' } }],
    })
    const w = mount(PolicyDetailView, {
      props: { name: 'p' },
      global: { plugins: [router], stubs: ['router-link', 'StatusBadge', 'TrendChart'] },
    })
    await flushPromises()
    expect(w.text()).toContain('Effectiveness')
    expect(w.text()).toContain('View as YAML')
  })
})
