import { mount, flushPromises } from '@vue/test-utils'
import { describe, it, expect, vi } from 'vitest'
import PoliciesView from './PoliciesView.vue'
import * as api from '../lib/api'
import { createRouter, createMemoryHistory } from 'vue-router'

vi.mock('../lib/api', async (o) => ({ ...((await o()) as object), api: vi.fn() }))

const kinds = [
  { name: 'Deployment', updateField: 'deployment', inPlaceOnly: false },
  { name: 'Job', updateField: 'job', inPlaceOnly: true },
  { name: 'Widget', updateField: 'widget', inPlaceOnly: false },
]

function serve(policies: unknown[]) {
  ;(api.api as any).mockImplementation(async (path: string) =>
    path === '/api/kinds' ? kinds : policies,
  )
}

async function mountView() {
  const router = createRouter({
    history: createMemoryHistory(),
    routes: [{ path: '/:catchAll(.*)', component: { template: '<div/>' } }],
  })
  const w = mount(PoliciesView, {
    global: { plugins: [router], stubs: ['router-link', 'StatusBadge'] },
  })
  await flushPromises()
  return w
}

describe('PoliciesView', () => {
  it('renders effectiveness columns', async () => {
    serve([
      {
        name: 'p',
        conditions: [],
        workloadCount: 5,
        cpuSavingsCores: 1.2,
        memSavingsBytes: 1e9,
        blockedCount: 3,
      },
    ])
    const w = await mountView()
    expect(w.text()).toContain('Workloads')
    expect(w.text()).toContain('CPU saved')
    expect(w.text()).toContain('1.20')
    expect(w.text()).toContain('Blocked')
    expect(w.find('td[data-label="Blocked"]').text()).toBe('3')
  })

  it('shows the update mode of every kind the server serves', async () => {
    serve([{ name: 'p', update: { widget: 'OnCreate', job: 'Ongoing', deployment: 'Ongoing' } }])
    const w = await mountView()
    expect(w.find('td[data-label="Mode"]').text()).toBe(
      'Deployment: Ongoing, Job: Ongoing (in place), Widget: OnCreate',
    )
  })
})
