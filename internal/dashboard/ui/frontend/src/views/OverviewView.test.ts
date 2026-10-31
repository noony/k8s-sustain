import { mount, flushPromises } from '@vue/test-utils'
import { describe, it, expect, vi } from 'vitest'
import OverviewView from './OverviewView.vue'
import * as api from '../lib/api'

vi.mock('../lib/api', async (orig) => ({
  ...((await orig()) as object),
  api: vi.fn(),
}))

const emptyGroup = { total: 0, rows: [] }

function summary(
  kpi: Partial<api.SummaryV2['kpi']> = {},
  policies: api.PolicyRollup[] = [],
  attention: Partial<api.SummaryV2['attention']> = {},
) {
  return {
    kpi: {
      cpuSavedCores: 0,
      cpuSavedRatio: 0,
      cpuSpark7d: [],
      memSavedBytes: 0,
      memSavedRatio: 0,
      memSpark7d: [],
      coordinatedCount: 0,
      ...kpi,
    },
    headroom: {
      cpu: { used: 0.4, idle: 0.3, free: 0.3 },
      memory: { used: 0.5, idle: 0.2, free: 0.3 },
    },
    attention: {
      conflicted: emptyGroup,
      blocked: emptyGroup,
      'at-risk': emptyGroup,
      drifted: emptyGroup,
      ...attention,
    },
    policies,
  }
}

function serve(payload: ReturnType<typeof summary>) {
  const apiMock = api.api as unknown as ReturnType<typeof vi.fn>
  apiMock.mockReset()
  apiMock.mockImplementation((path: string) => {
    if (path === '/api/summary') return Promise.resolve(payload)
    if (path.startsWith('/api/summary/trend'))
      return Promise.resolve({
        cpu: { usage: [], request: [], originalRequest: [] },
        memory: { usage: [], request: [], originalRequest: [] },
      })
    if (path.startsWith('/api/summary/activity')) return Promise.resolve({ items: [] })
    return Promise.reject(new Error(`unexpected request ${path}`))
  })
  return apiMock
}

describe('OverviewView', () => {
  it('renders KPI strip, headroom, attention queue, policies', async () => {
    serve(
      summary({ cpuSavedCores: 3.2, cpuSavedRatio: 0.18, cpuSpark7d: [1, 2, 3] }, [
        { name: 'p', workloadCount: 1, cpuSavingsCores: 0.5, memSavingsBytes: 1, blockedCount: 0 },
      ]),
    )
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

  it('renders every Risk-state and Coordinated count from the backend', async () => {
    const apiMock = serve(
      summary({ coordinatedCount: 7 }, [], {
        conflicted: { total: 3, rows: [] },
        blocked: { total: 4, rows: [] },
        'at-risk': { total: 1, rows: [] },
        drifted: { total: 2, rows: [] },
      }),
    )
    const w = mount(OverviewView, { global: { stubs: ['router-link', 'TrendChart'] } })
    await flushPromises()
    const cards = w.findAllComponents({ name: 'KpiCard' })
    const byLabel = (label: string) => cards.find((c) => c.props('label') === label)
    expect(cards.map((c) => c.props('label')).slice(2, 6)).toEqual([
      'Conflicted',
      'Blocked',
      'At risk',
      'Drifted',
    ])
    for (const [label, value, to] of [
      ['Conflicted', '3', '/workloads?risk=conflicted'],
      ['Blocked', '4', '/workloads?risk=blocked'],
      ['At risk', '1', '/workloads?risk=at-risk'],
      ['Drifted', '2', '/workloads?risk=drifted'],
    ]) {
      expect(byLabel(label)?.props('value')).toBe(value)
      expect(byLabel(label)?.props('to')).toBe(to)
    }
    expect(byLabel('Blocked')?.props('detail')).toBe('last apply failed')
    expect(byLabel('Coordinated')?.props('value')).toBe('7')
    const paths = apiMock.mock.calls.map(([path]) => path as string)
    expect(paths.some((p) => p.startsWith('/api/workloads'))).toBe(false)
  })
})
