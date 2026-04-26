import { mount, flushPromises } from '@vue/test-utils'
import { describe, it, expect, vi } from 'vitest'
import { createRouter, createMemoryHistory } from 'vue-router'
import WorkloadsView from './WorkloadsView.vue'
import * as api from '../lib/api'
vi.mock('../lib/api', async (o) => ({ ...((await o()) as object), api: vi.fn() }))

const router = createRouter({
  history: createMemoryHistory(),
  routes: [{ path: '/', component: WorkloadsView }],
})

describe('WorkloadsView', () => {
  it('renders new risk/drift columns', async () => {
    ;(api.api as any).mockResolvedValue({
      items: [
        {
          namespace: 'a',
          kind: 'Deployment',
          name: 'web',
          containers: [],
          automated: true,
          riskState: 'at-risk',
          driftPercent: 18.4,
          autoscalerPresent: true,
        },
      ],
      total: 1,
      pageSize: 50,
      namespaces: ['a'],
      kinds: ['Deployment'],
      counts: { total: 1, automated: 1, manual: 0 },
    })
    const w = mount(WorkloadsView, { global: { plugins: [router] } })
    await flushPromises()
    expect(w.text()).toContain('Risk')
    expect(w.text()).toContain('Drift')
    expect(w.text()).toContain('At risk')
    expect(w.text()).toContain('18.4')
  })
})
