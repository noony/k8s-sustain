import { mount, flushPromises } from '@vue/test-utils'
import { describe, it, expect, vi, afterEach } from 'vitest'
import SimulatorView from './SimulatorView.vue'
import * as api from '../lib/api'
vi.mock('../lib/api', async (o) => ({ ...((await o()) as object), api: vi.fn() }))

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
  cpuSeries: {},
  memorySeries: {},
  liveOOMWindowSeconds: 1800,
})

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
  afterEach(() => vi.useRealTimers())

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
