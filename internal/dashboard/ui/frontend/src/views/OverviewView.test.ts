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
            blockedCount: 4,
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
              blockedCount: 0,
            },
          ],
        })
      if (path.startsWith('/api/summary/trend'))
        return Promise.resolve({
          cpu: { usage: [], request: [], originalRequest: [] },
          memory: { usage: [], request: [], originalRequest: [] },
        })
      if (path.startsWith('/api/summary/activity')) return Promise.resolve({ items: [] })
      return Promise.resolve({})
    })
    const w = mount(OverviewView, { global: { stubs: ['router-link', 'TrendChart'] } })
    await flushPromises()
    expect(w.text()).toContain('CPU saved')
    expect(w.findComponent({ name: 'HeadroomBar' }).exists()).toBe(true)
    expect(w.findComponent({ name: 'AttentionQueue' }).exists()).toBe(true)
    expect(w.text()).toContain('p') // policy row
    expect(w.text()).not.toContain('Cluster savings')
    const headers = w.findAll('.card-header h2').map((h) => h.text())
    expect(headers).toContain('Savings')
    expect(w.text()).toContain('CPU')
    expect(w.text()).toContain('Memory')
  })

  it('shows Blocked and At risk as separate identity counts', async () => {
    const apiMock = api.api as unknown as ReturnType<typeof vi.fn>
    apiMock.mockImplementation((path: string) => {
      if (path === '/api/summary')
        return Promise.resolve({
          kpi: {
            cpuSavedCores: 0,
            cpuSavedRatio: 0,
            cpuSpark7d: [],
            memSavedBytes: 0,
            memSavedRatio: 0,
            memSpark7d: [],
            atRiskCount: 1,
            blockedCount: 4,
            driftedCount: 2,
          },
          headroom: {
            cpu: { used: 0, idle: 0, free: 0 },
            memory: { used: 0, idle: 0, free: 0 },
          },
          attention: { risk: [], drift: [], blocked: [] },
          policies: [],
        })
      if (path.startsWith('/api/summary/trend'))
        return Promise.resolve({
          cpu: { usage: [], request: [], originalRequest: [] },
          memory: { usage: [], request: [], originalRequest: [] },
        })
      if (path.startsWith('/api/summary/activity')) return Promise.resolve({ items: [] })
      return Promise.resolve({})
    })
    const w = mount(OverviewView, { global: { stubs: ['router-link', 'TrendChart'] } })
    await flushPromises()
    const cards = w.findAllComponents({ name: 'KpiCard' })
    const byLabel = (label: string) => cards.find((c) => c.props('label') === label)
    expect(byLabel('At risk')?.props('value')).toBe('1')
    expect(byLabel('At risk')?.props('to')).toBe('/workloads?risk=at-risk')
    expect(byLabel('Blocked')?.props('value')).toBe('4')
    expect(byLabel('Blocked')?.props('to')).toBe('/workloads?risk=blocked')
  })
})
