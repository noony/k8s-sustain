import { mount, flushPromises } from '@vue/test-utils'
import { describe, it, expect, vi, afterEach } from 'vitest'
import SimulatorView from './SimulatorView.vue'
import * as api from '../lib/api'
import * as chart from '../lib/chart'
vi.mock('../lib/api', async (o) => ({ ...((await o()) as object), api: vi.fn() }))
vi.mock('../lib/chart', async (o) => ({
  ...((await o()) as object),
  createTimeSeriesChart: vi.fn(),
}))

// What the server answers for web under policy p: its effective settings
// differ from every value the recommender would default.
const simulation = (): api.SimulationResult => ({
  policy: 'p',
  effective: {
    resourcesConfigs: {
      cpu: { window: '96h', requests: { percentile: 90, headroom: 10 }, limits: { noLimit: true } },
      memory: { window: '168h', requests: { keepRequest: true }, limits: { keepLimit: true } },
    },
    autoscalerCoordination: { enabled: true },
  },
  outcome: 'Computed',
  containers: { app: { cpuRequest: '100m' } },
  trace: { app: { cpu: { request: [{ stage: 'percentile', value: '90m' }] } } },
  liveOOMWindowSeconds: 1800,
})

const t0 = '2026-10-05T11:00:00Z'
const t1 = '2026-10-05T12:00:00Z'
const at = (timestamp: string, value: number) => ({ timestamp, value })

// web's history over two instants: memory's limit is recommended at the
// second only, and the pods' memory limit is known only from the spec.
const withHistory = (): api.SimulationResult => ({
  ...simulation(),
  resources: { app: { memoryLimit: '256Mi' } },
  history: {
    stepSeconds: 3600,
    containers: {
      app: {
        cpu: {
          usage: [at(t0, 0.08), at(t1, 0.09)],
          recommendedRequest: [at(t0, 0.088), at(t1, 0.099)],
          runningRequest: [at(t0, 0.2), at(t1, 0.2)],
          runningLimit: [at(t0, 1), at(t1, 1)],
        },
        memory: {
          usage: [at(t0, 64), at(t1, 64)],
          recommendedRequest: [at(t0, 64), at(t1, 200)],
          recommendedLimit: [at(t1, 400)],
          runningRequest: [at(t0, 128), at(t1, 128)],
        },
      },
    },
    oomKills: [{ timestamp: t1, container: 'app' }],
  },
})

// What each chart drew: its usage, then each other line by label.
function drawn() {
  return Object.fromEntries(
    (chart.createTimeSeriesChart as any).mock.calls.map(
      ([id, usage, opts]: [string, api.TimeValue[], chart.ChartOpts]) => [
        id,
        {
          usage,
          lines: Object.fromEntries((opts.extraSeries ?? []).map((s) => [s.label, s.data])),
          annotations: (opts.annotations ?? []).map((a) => a.label),
          oomEvents: opts.oomEvents,
          stepMs: opts.stepMs,
        },
      ],
    ),
  )
}

function serveSimulator(simulate: (body: api.SimulateRequest) => Promise<unknown>) {
  const bodies: api.SimulateRequest[] = []
  ;(api.api as any).mockReset()
  ;(api.api as any).mockImplementation(async (path: string, opts?: RequestInit) => {
    if (path === '/api/simulate') {
      const body = JSON.parse(opts!.body as string)
      bodies.push(body)
      return simulate(body)
    }
    if (path.startsWith('/api/workloads?')) return { items: [], total: 0, pageSize: 200 }
    return []
  })
  return bodies
}

function mountOpened() {
  return mount(SimulatorView, {
    props: { namespace: 'prod', kind: 'Deployment', name: 'web' },
    global: { stubs: ['TimeRangePicker'] },
  })
}

describe('SimulatorView', () => {
  afterEach(() => {
    vi.useRealTimers()
    ;(chart.createTimeSeriesChart as any).mockClear()
  })

  it('charts, per resource, the recommendation computed at each instant over the usage it read', async () => {
    serveSimulator(async () => withHistory())
    mountOpened()
    await flushPromises()

    const charts = drawn()
    expect(Object.keys(charts).sort()).toEqual(['simcpu-app', 'simmem-app'])
    expect(charts['simcpu-app']).toEqual({
      usage: [at(t0, 0.08), at(t1, 0.09)],
      lines: {
        Request: [at(t0, 0.2), at(t1, 0.2)],
        Limit: [at(t0, 1), at(t1, 1)],
        'Recommended request': [at(t0, 0.088), at(t1, 0.099)],
      },
      annotations: [],
      oomEvents: undefined,
      stepMs: 3600_000,
    })
    expect(charts['simmem-app']).toEqual({
      usage: [at(t0, 64), at(t1, 64)],
      lines: {
        Request: [at(t0, 128), at(t1, 128)],
        'Recommended request': [at(t0, 64), at(t1, 200)],
        'Recommended limit': [at(t1, 400)],
      },
      annotations: ['Limit: 256Mi'],
      oomEvents: [{ timestamp: t1, container: 'app' }],
      stepMs: 3600_000,
    })
  })

  it("says coordination uses today's autoscaler", async () => {
    serveSimulator(async () => withHistory())
    const w = mountOpened()
    await flushPromises()

    expect(w.find('[data-test="history-legend"]').text()).toContain("today's autoscaler")
  })

  it('keeps the numbers and shows why when the history is unavailable', async () => {
    serveSimulator(async () => ({ ...simulation(), historyError: 'too many samples' }))
    const w = mountOpened()
    await flushPromises()

    expect(w.find('[data-test="history-error"]').text()).toBe(
      'History unavailable: too many samples',
    )
    expect(w.find('.rec-card').exists()).toBe(true)
    expect(chart.createTimeSeriesChart).not.toHaveBeenCalled()
  })

  it('opens on the governing policy: sends no override and shows its effective settings', async () => {
    const bodies = serveSimulator(async () => simulation())
    const w = mountOpened()
    await flushPromises()

    expect(bodies).toHaveLength(1)
    expect(bodies[0].override).toEqual({})
    expect(bodies[0].policy).toBeUndefined()
    expect(w.find('[data-test="starting-point"]').text()).toContain('policy p')
    expect((w.find('[data-test="cpu-percentile"]').element as HTMLInputElement).value).toBe('90')
    expect((w.find('#cpu-limit-mode').element as HTMLSelectElement).value).toBe('noLimit')
    expect((w.find('[data-test="memory-keep-request"]').element as HTMLInputElement).checked).toBe(
      true,
    )
    expect(w.find('[data-test="memory-percentile"]').exists()).toBe(false)
    expect((w.find('[data-test="coordination"]').element as HTMLInputElement).checked).toBe(true)
    expect(w.text()).toContain('Usage percentile')
  })

  it('sends only what the user changed', async () => {
    vi.useFakeTimers()
    const bodies = serveSimulator(async () => simulation())
    const w = mountOpened()
    await flushPromises()

    await w.find('[data-test="cpu-percentile"]').setValue('80')
    await w.find('#memory-limit-mode').setValue('equalsToRequest')
    vi.advanceTimersByTime(400)
    await flushPromises()

    expect(bodies).toHaveLength(2)
    expect(bodies[1].override).toEqual({
      resourcesConfigs: {
        cpu: { requests: { percentile: 80 } },
        memory: {
          limits: {
            equalsToRequest: true,
            keepLimit: false,
            keepLimitRequestRatio: false,
            noLimit: false,
            requestsLimitsRatio: null,
          },
        },
      },
    })
    expect((w.find('[data-test="cpu-percentile"]').element as HTMLInputElement).value).toBe('80')
  })

  it('sends the workload settings and a kept request, and keeps them over the response', async () => {
    vi.useFakeTimers()
    const bodies = serveSimulator(async () => simulation())
    const w = mountOpened()
    await flushPromises()

    await w.find('[data-test="coordination"]').setValue(false)
    await w.find('[data-test="cpu-keep-request"]').setValue(true)
    vi.advanceTimersByTime(400)
    await flushPromises()

    expect(bodies[1].override).toEqual({
      autoscalerCoordination: { enabled: false },
      resourcesConfigs: {
        cpu: {
          requests: {
            keepRequest: true,
            percentile: null,
            headroom: null,
            minAllowed: null,
            maxAllowed: null,
          },
        },
      },
    })
    expect((w.find('[data-test="coordination"]').element as HTMLInputElement).checked).toBe(false)
    expect(w.find('[data-test="cpu-percentile"]').exists()).toBe(false)
  })

  it("explains each resource's limit strategy", async () => {
    serveSimulator(async () => simulation())
    const w = mountOpened()
    await flushPromises()

    expect(w.find('#cpu-limit-help').text()).toBe('Strips the CPU limit from the pod.')
    expect(w.find('#memory-limit-help').text()).toBe(
      'Each pod keeps its limit, raised to the request when below it.',
    )
    await w.find('#memory-limit-mode').setValue('equalsToRequest')
    expect(w.find('#memory-limit-help').text()).toContain('triggers OOM-kill')
  })

  it('names the live OOM window the server sends', async () => {
    serveSimulator(async () => ({ ...simulation(), liveOOMWindowSeconds: 3600 }))
    const w = mountOpened()
    await flushPromises()

    expect(w.text()).toContain("the controller's watcher saw in the last 1 hour,")
  })

  it("shows the server's rejection of an override and keeps the last results", async () => {
    vi.useFakeTimers()
    let calls = 0
    serveSimulator(async () => {
      if (calls++ === 0) return simulation()
      throw new api.ApiError(
        'invalid override: resourcesConfigs.cpu.requests: minAllowed must be less than or equal to maxAllowed',
        {
          code: 'BAD_REQUEST',
          status: 400,
          field: 'resourcesConfigs.cpu.requests',
        },
      )
    })
    const w = mountOpened()
    await flushPromises()

    await w.find('[data-test="cpu-percentile"]').setValue('70')
    vi.advanceTimersByTime(400)
    await flushPromises()

    expect(w.find('[data-test="override-error"]').text()).toContain('minAllowed must be')
    expect(w.find('.rec-card').exists()).toBe(true)
  })

  it('counts a kept request as no change in the savings', async () => {
    serveSimulator(async () => ({
      ...simulation(),
      resources: { app: { cpuRequest: '200m', memoryRequest: '256Mi' } },
    }))
    const w = mountOpened()
    await flushPromises()

    const kpi = (label: string) =>
      w
        .findAll('.kpi-card')
        .find((c) => c.find('.kpi-label').text() === label)!
        .find('.kpi-value')
        .text()
    expect(kpi('CPU change')).toBe('-0.10 cores')
    expect(kpi('Memory change')).toBe('0 Mi')
  })

  it('reset drops every edit and starts again from the policy', async () => {
    vi.useFakeTimers()
    const bodies = serveSimulator(async () => simulation())
    const w = mountOpened()
    await flushPromises()

    await w.find('[data-test="cpu-percentile"]').setValue('80')
    await w.find('[data-test="reset-override"]').trigger('click')
    vi.advanceTimersByTime(400)
    await flushPromises()

    expect(bodies.at(-1)!.override).toEqual({})
    expect((w.find('[data-test="cpu-percentile"]').element as HTMLInputElement).value).toBe('90')
  })

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
