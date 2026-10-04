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

function serve(snapshot: object) {
  ;(api.api as any).mockImplementation((path: string) => {
    if (path.endsWith('/metrics?window=1w&step=20m'))
      return Promise.resolve({ cpu: {}, memory: {}, initContainers: ['migrate'] })
    if (path.match(/\/api\/workloads\/[^/]+\/[^/]+\/[^/]+$/))
      return Promise.resolve({ automated: false, departed: false, recentEvents: [], ...snapshot })
    return Promise.reject(new Error('unexpected ' + path))
  })
}

function mountDetail() {
  return mount(WorkloadDetailView, {
    props: { namespace: 'a', kind: 'Deployment', name: 'web' },
    global: {
      plugins: [router],
      stubs: ['TimeRangePicker', 'TrendChart'],
    },
  })
}

describe('WorkloadDetailView', () => {
  it('renders status snapshot', async () => {
    serve({
      updateMode: 'Ongoing',
      riskState: 'at-risk',
      oom24h: 2,
      stalePods: 1,
      totalPods: 4,
    })
    const w = mountDetail()
    await flushPromises()
    expect(w.text()).toContain('Ongoing')
    expect(w.text()).toContain('OOM')
    expect(w.text()).toContain('1/4 pods')
    expect(w.findComponent({ name: 'RiskBadge' }).props('state')).toBe('at-risk')
  })

  it('renders the risk state the backend classified, not one derived from the signals', async () => {
    serve({
      riskState: 'blocked',
      oom24h: 3,
      stalePods: 0,
      totalPods: 2,
      blocked: { reason: 'patch', attempts: 2 },
    })
    const w = mountDetail()
    await flushPromises()
    expect(w.findComponent({ name: 'RiskBadge' }).text()).toBe('Blocked')
  })

  it('shows the stored Recommendation and its outcome without recomputing it', async () => {
    serve({
      automated: true,
      policyName: 'p',
      riskState: 'safe',
      oom24h: 0,
      stalePods: 0,
      totalPods: 1,
      recommendation: {
        outcome: 'NoData',
        observedAt: new Date(Date.now() - 3600 * 1000).toISOString(),
        containers: {
          app: { cpuRequest: '250m', memoryRequest: '128Mi' },
          migrate: { cpuRequest: '50m' },
        },
      },
    })
    const w = mountDetail()
    await flushPromises()
    expect(w.find('[data-test="outcome"]').text()).toBe('No data')
    expect(w.text()).toContain('250m')
    expect(w.text()).toContain('128Mi')
    expect(w.text()).toContain('computed')
    const paths = (api.api as any).mock.calls.map((c: unknown[]) => c[0] as string)
    expect(paths.some((p: string) => p.includes('/recommendations'))).toBe(false)
  })

  it('shows how each container was computed from the stored trace', async () => {
    serve({
      automated: true,
      policyName: 'p',
      riskState: 'safe',
      oom24h: 0,
      stalePods: 0,
      totalPods: 1,
      recommendation: {
        outcome: 'Computed',
        containers: { app: { cpuRequest: '414m', memoryRequest: '256Mi' } },
        trace: {
          app: {
            cpu: {
              percentile: '123400u',
              withHeadroom: '136m',
              clamped: '150m',
              coordination: {
                overheadFactor: 1.375,
                replicaFactor: 2,
                scaled: '414m',
                value: '414m',
              },
            },
            memory: {
              percentile: '100Mi',
              oomFloor: { value: '240Mi', determined: true },
              withHeadroom: '240Mi',
              clamped: '240Mi',
              coordination: { overheadFactor: 1.375, scaled: '330Mi', value: '256Mi' },
            },
          },
        },
      },
    })
    const w = mountDetail()
    await flushPromises()
    const trace = w.find('[data-test="trace"]')
    expect(trace.exists()).toBe(true)
    expect(trace.text()).toContain('123.4m')
    expect(trace.text()).toContain('×2 replica')
    expect(trace.text()).toContain('240Mi · set the request')
    expect(trace.text()).toContain('clamped to 256Mi')
  })

  it('names the Policies of a Conflicted identity', async () => {
    serve({
      conflictingPolicies: ['p', 'q'],
      riskState: 'conflicted',
      oom24h: 0,
      stalePods: 0,
      totalPods: 0,
    })
    const w = mountDetail()
    await flushPromises()
    expect(w.text()).toContain('Conflicted: p / q')
    expect(w.findComponent({ name: 'RiskBadge' }).text()).toBe('Conflicted')
  })
})
