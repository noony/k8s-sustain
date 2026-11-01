import { mount, flushPromises } from '@vue/test-utils'
import { describe, it, expect, vi } from 'vitest'
import { createRouter, createMemoryHistory } from 'vue-router'
import WorkloadDetailView from './WorkloadDetailView.vue'
import * as api from '../lib/api'
import * as chart from '../lib/chart'
vi.mock('../lib/api', async (o) => ({ ...((await o()) as object), api: vi.fn() }))
vi.mock('../lib/chart', async (o) => ({
  ...((await o()) as object),
  createTimeSeriesChart: vi.fn(),
}))

const router = createRouter({
  history: createMemoryHistory(),
  routes: [{ path: '/', component: WorkloadDetailView }],
})

function serve(snapshot: object, metrics: object = {}) {
  ;(api.api as any).mockImplementation((path: string) => {
    if (path.endsWith('/metrics?window=1w&step=20m'))
      return Promise.resolve({ cpu: {}, memory: {}, initContainers: ['migrate'], ...metrics })
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

  it('draws the stored request and limit, and shows them next to what the pods run with', async () => {
    ;(chart.createTimeSeriesChart as any).mockClear()
    const usage = [{ timestamp: '2026-10-05T12:00:00Z', value: 1 }]
    serve(
      {
        automated: true,
        policyName: 'p',
        riskState: 'safe',
        oom24h: 0,
        stalePods: 0,
        totalPods: 1,
        recommendation: {
          outcome: 'Computed',
          containers: {
            app: {
              cpuRequest: '250m',
              cpuLimitRemoved: true,
              memoryRequest: '128Mi',
              memoryLimit: '256Mi',
            },
          },
        },
      },
      {
        cpu: { app: usage },
        memory: { app: usage },
        resources: { app: { cpuLimit: '1', memoryLimit: '192Mi' } },
      },
    )
    const w = mountDetail()
    await flushPromises()

    const annotations = Object.fromEntries(
      (chart.createTimeSeriesChart as any).mock.calls.map(
        ([id, , opts]: [string, unknown, chart.ChartOpts]) => [
          id,
          (opts.annotations ?? []).filter((a) => a.color === 'rec').map((a) => [a.label, a.value]),
        ],
      ),
    )
    expect(annotations).toEqual({
      'cpu-app': [['Recommended request: 250m', 0.25]],
      'mem-app': [
        ['Recommended request: 128Mi', 128 * 1024 * 1024],
        ['Recommended limit: 256Mi', 256 * 1024 * 1024],
      ],
    })
    const rows = Object.fromEntries(
      w.findAll('.resource-row').map((r) => [r.find('.label').text(), r.text()]),
    )
    expect(rows['CPU Limit']).toContain('removed')
    expect(rows['Memory Limit']).toContain('192Mi')
    expect(rows['Memory Limit']).toContain('256Mi')
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
              request: [
                { stage: 'percentile', value: '123400u' },
                { stage: 'headroom', value: '136m' },
                { stage: 'clamp', value: '150m' },
                {
                  stage: 'coordination',
                  value: '414m',
                  facts: { overheadFactor: '1.375', replicaFactor: '2' },
                },
              ],
            },
            memory: {
              request: [
                { stage: 'percentile', value: '100Mi' },
                { stage: 'oomFloor', value: '240Mi', facts: { determined: 'true' } },
                { stage: 'headroom', value: '240Mi' },
                { stage: 'clamp', value: '240Mi' },
              ],
              limit: [
                { stage: 'keepLimit', value: '200Mi' },
                { stage: 'guard', value: '240Mi' },
              ],
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
    expect(trace.text()).toContain('replicaFactor: 2')
    expect(trace.text()).toContain('240Mi · determined: true')
    expect(trace.text()).toContain('Limit raised to the request')
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

  describe('usage lines', () => {
    const average = [{ timestamp: '2026-10-05T12:00:00Z', value: 0.2 }]
    const busiest = [
      { timestamp: '2026-10-05T11:20:00Z', value: 0.5 },
      { timestamp: '2026-10-05T12:00:00Z', value: 0.7 },
    ]
    const governed = {
      automated: true,
      policyName: 'p',
      riskState: 'safe',
      oom24h: 0,
      stalePods: 0,
      totalPods: 1,
    }
    const signal = {
      policy: 'p',
      stepSeconds: 2400,
      cpu: { percentile: 90, window: '24h' },
      memory: { percentile: 95, window: '168h' },
      containers: { app: { cpu: busiest, memory: busiest } },
    }

    function drawn() {
      return Object.fromEntries(
        (chart.createTimeSeriesChart as any).mock.calls.map(
          ([id, points, opts]: [string, unknown, chart.ChartOpts]) => [
            id,
            [[opts.label, points], ...(opts.extraSeries ?? []).map((s) => [s.label, s.data])],
          ],
        ),
      )
    }

    function notes() {
      return (chart.createTimeSeriesChart as any).mock.calls.flatMap(
        ([, , opts]: [string, unknown, chart.ChartOpts]) => opts.notes ?? [],
      )
    }

    it('draws the replica average and what the usage signal reads on each chart', async () => {
      ;(chart.createTimeSeriesChart as any).mockClear()
      serve(governed, { cpu: { app: average }, memory: { app: average }, signal })
      mountDetail()
      await flushPromises()

      expect(drawn()).toEqual({
        'cpu-app': [
          ['Usage (replica average)', average],
          ['Busiest replica, p90 over 24h: what the recommender reads', busiest],
        ],
        'mem-app': [
          ['Usage (replica average)', average],
          ['Busiest replica, p95 over 168h: what the recommender reads', busiest],
        ],
      })
      const signalSeries = (chart.createTimeSeriesChart as any).mock.calls[0][2].extraSeries[0]
      expect(signalSeries.stepMs).toBe(2400 * 1000)
      expect(notes()).toEqual([])
    })

    it.each([
      {
        why: 'the signal history could not be read',
        snapshot: governed,
        metrics: { signal: null, signalError: 'too many samples' },
        want: [
          'Signal history unavailable: too many samples',
          'Signal history unavailable: too many samples',
        ],
      },
      {
        why: 'no Policy governs an identity the backend classified Conflicted',
        snapshot: { riskState: 'conflicted' },
        metrics: { signal: null },
        want: [
          'No signal line: Conflicted, no Policy governs this workload.',
          'No signal line: Conflicted, no Policy governs this workload.',
        ],
      },
      {
        why: 'no Policy governs the identity',
        snapshot: { riskState: 'safe' },
        metrics: { signal: null },
        want: [
          'No signal line: no Policy governs this workload.',
          'No signal line: no Policy governs this workload.',
        ],
      },
      {
        why: 'the Policy keeps the memory request',
        snapshot: governed,
        metrics: {
          signal: {
            ...signal,
            memory: { window: '168h', keepRequest: true },
            containers: { app: { cpu: busiest } },
          },
        },
        want: ['No signal line: the Policy keeps the memory request.'],
      },
    ])(
      'says in the chart why it has no signal line when $why',
      async ({ snapshot, metrics, want }) => {
        ;(chart.createTimeSeriesChart as any).mockClear()
        serve(
          { oom24h: 0, stalePods: 0, totalPods: 1, ...snapshot },
          { cpu: { app: average }, memory: { app: average }, ...metrics },
        )
        mountDetail()
        await flushPromises()

        expect(notes()).toEqual(want)
        expect(drawn()['mem-app']).toEqual([['Usage (replica average)', average]])
      },
    )
  })
})
