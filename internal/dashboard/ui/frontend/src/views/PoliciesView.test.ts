import { mount, flushPromises } from '@vue/test-utils'
import { describe, it, expect, vi } from 'vitest'
import PoliciesView from './PoliciesView.vue'
import * as api from '../lib/api'
import { createRouter, createMemoryHistory } from 'vue-router'

vi.mock('../lib/api', async (o) => ({ ...((await o()) as object), api: vi.fn() }))

describe('PoliciesView', () => {
  it('renders effectiveness columns', async () => {
    ;(api.api as any).mockResolvedValue([
      {
        name: 'p',
        conditions: [],
        workloadCount: 5,
        cpuSavingsCores: 1.2,
        memSavingsBytes: 1e9,
        atRiskCount: 1,
      },
    ])
    const router = createRouter({
      history: createMemoryHistory(),
      routes: [{ path: '/:catchAll(.*)', component: { template: '<div/>' } }],
    })
    const w = mount(PoliciesView, {
      global: { plugins: [router], stubs: ['router-link', 'StatusBadge'] },
    })
    await flushPromises()
    expect(w.text()).toContain('Workloads')
    expect(w.text()).toContain('CPU saved')
    expect(w.text()).toContain('1.20')
  })
})
