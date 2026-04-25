import { mount, flushPromises } from '@vue/test-utils'
import { describe, it, expect, vi } from 'vitest'
import OverviewView from './OverviewView.vue'
import * as api from '../lib/api'

vi.mock('../lib/api', async (orig) => ({
  ...((await orig()) as object),
  api: vi.fn(),
}))

describe('OverviewView', () => {
  it('renders KPI strip, headroom, attention queue, policies', async () => {
    const apiMock = api.api as unknown as ReturnType<typeof vi.fn>
    apiMock.mockImplementation((path: string) => {
      if (path === '/api/summary')
        return Promise.resolve({
          kpi: {
            cpuSavedCores: 3.2,
            cpuSavedRatio: 0.18,
            cpuSpark7d: [1, 2, 3],
            memSavedBytes: 1,
            memSavedRatio: 0.1,
            memSpark7d: [1],
            atRiskCount: 1,
            driftedCount: 2,
          },
          headroom: {
            cpu: { used: 0.4, idle: 0.3, free: 0.3 },
            memory: { used: 0.5, idle: 0.2, free: 0.3 },
          },
          attention: { risk: [], drift: [], blocked: [] },
          policies: [
            {
              name: 'p',
              workloadCount: 1,
              cpuSavingsCores: 0.5,
              memSavingsBytes: 1,
              atRiskCount: 0,
            },
          ],
        })
      if (path.startsWith('/api/summary/trend')) return Promise.resolve({ cpu: [], memory: [] })
      if (path.startsWith('/api/summary/activity')) return Promise.resolve({ items: [] })
      return Promise.resolve({})
    })
    const w = mount(OverviewView, { global: { stubs: ['router-link', 'TrendChart'] } })
    await flushPromises()
    expect(w.text()).toContain('CPU saved')
    expect(w.findComponent({ name: 'HeadroomBar' }).exists()).toBe(true)
    expect(w.findComponent({ name: 'AttentionQueue' }).exists()).toBe(true)
    expect(w.text()).toContain('p') // policy row
  })
})
