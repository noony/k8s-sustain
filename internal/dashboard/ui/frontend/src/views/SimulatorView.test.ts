import { mount, flushPromises } from '@vue/test-utils'
import { describe, it, expect, vi } from 'vitest'
import SimulatorView from './SimulatorView.vue'
import * as api from '../lib/api'
vi.mock('../lib/api', async (o) => ({ ...((await o()) as object), api: vi.fn() }))

describe('SimulatorView', () => {
  it('renders form with Rollout option', async () => {
    ;(api.api as any).mockResolvedValue([])
    const w = mount(SimulatorView, { global: { stubs: ['TimeRangePicker'] } })
    await flushPromises()
    expect(w.html()).toContain('<option value="Rollout">')
  })

  it('pages through every workload within the API page-size limit', async () => {
    const row = (i: number) => ({ namespace: `ns-${i}`, kind: 'Deployment', name: `w-${i}` })
    const pages: Record<string, unknown[]> = {
      '1': Array.from({ length: 200 }, (_, i) => row(i)),
      '2': Array.from({ length: 50 }, (_, i) => row(200 + i)),
    }
    ;(api.api as any).mockReset()
    ;(api.api as any).mockImplementation(async (path: string) => {
      if (!path.startsWith('/api/workloads?')) return []
      const q = new URLSearchParams(path.split('?')[1])
      if (Number(q.get('pageSize')) > 200) throw new Error('pageSize over the API limit')
      return { items: pages[q.get('page') || '1'] || [], total: 250, pageSize: 200 }
    })
    const w = mount(SimulatorView, { global: { stubs: ['TimeRangePicker'] } })
    await flushPromises()
    const workloadCalls = (api.api as any).mock.calls
      .map((c: unknown[]) => c[0] as string)
      .filter((p: string) => p.startsWith('/api/workloads?'))
    expect(workloadCalls).toHaveLength(2)
    const nsCombobox = w.findAllComponents({ name: 'Combobox' })[0]
    expect(nsCombobox.props('options')).toHaveLength(250)
  })
})
