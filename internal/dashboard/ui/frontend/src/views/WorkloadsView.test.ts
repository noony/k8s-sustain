import { mount, flushPromises, type VueWrapper } from '@vue/test-utils'
import { describe, it, expect, vi, beforeEach } from 'vitest'
import { createRouter, createMemoryHistory } from 'vue-router'
import WorkloadsView from './WorkloadsView.vue'
import * as api from '../lib/api'
vi.mock('../lib/api', async (o) => ({ ...((await o()) as object), api: vi.fn() }))

const router = createRouter({
  history: createMemoryHistory(),
  routes: [{ path: '/', component: WorkloadsView }],
})

const emptyList = {
  items: [],
  total: 0,
  pageSize: 50,
  namespaces: ['a', 'b'],
  kinds: ['Deployment'],
  counts: { total: 0, automated: 0, manual: 0 },
}

async function mountAt(url: string) {
  const r = createRouter({
    history: createMemoryHistory(),
    routes: [
      { path: '/workloads', component: WorkloadsView },
      { path: '/workloads/:namespace/:kind/:name', component: { template: '<div />' } },
    ],
  })
  await r.push(url)
  await r.isReady()
  const w = mount(WorkloadsView, { global: { plugins: [r] } })
  await flushPromises()
  return { w, r }
}

const oneRow = {
  ...emptyList,
  items: [{ namespace: 'a', kind: 'Deployment', name: 'web', containers: [] }],
  total: 1,
}

function header(w: VueWrapper, label: string) {
  return w.findAll('th').find(
    (th) =>
      th
        .text()
        .replace(/[\u25B2\u25BC]/, '')
        .trim() === label,
  )!
}

function lastApiParams(): URLSearchParams {
  const calls = (api.api as any).mock.calls
  const url = calls[calls.length - 1][0] as string
  return new URLSearchParams(url.split('?')[1])
}

describe('WorkloadsView', () => {
  beforeEach(() => {
    ;(api.api as any).mockReset()
    ;(api.api as any).mockResolvedValue(emptyList)
  })

  it('sorts by name by default', async () => {
    ;(api.api as any).mockResolvedValue(oneRow)
    const { w, r } = await mountAt('/workloads')
    expect(lastApiParams().get('sort')).toBe('name')
    expect(r.currentRoute.value.query.sort).toBeUndefined()
    expect(header(w, 'Name').text()).toContain('\u25B2')
    expect(header(w, 'Namespace').text()).not.toMatch(/[\u25B2\u25BC]/)
  })

  it('restores filters, page and sort from the query string', async () => {
    await mountAt('/workloads?namespace=a&risk=at-risk&search=web&page=2&sort=-kind')
    const p = lastApiParams()
    expect(p.get('namespace')).toBe('a')
    expect(p.get('risk')).toBe('at-risk')
    expect(p.get('search')).toBe('web')
    expect(p.get('page')).toBe('2')
    expect(p.get('sort')).toBe('-kind')
  })

  it('writes filter changes to the query string and resets the page', async () => {
    const { w, r } = await mountAt('/workloads?page=3&kind=Deployment')
    const riskSelect = w.findAll('select').find((s) => s.text().includes('Any risk'))!
    await riskSelect.setValue('blocked')
    await flushPromises()
    expect(r.currentRoute.value.query).toEqual({ kind: 'Deployment', risk: 'blocked' })
    expect(lastApiParams().get('risk')).toBe('blocked')
    expect(lastApiParams().get('page')).toBe('1')
  })

  it('debounces search into the query string', async () => {
    vi.useFakeTimers()
    try {
      const { w, r } = await mountAt('/workloads')
      await w.find('input[type="text"][placeholder^="Search"]').setValue('api')
      expect(r.currentRoute.value.query.search).toBeUndefined()
      vi.advanceTimersByTime(300)
      await flushPromises()
      expect(r.currentRoute.value.query.search).toBe('api')
      expect(lastApiParams().get('search')).toBe('api')
    } finally {
      vi.useRealTimers()
    }
  })

  it('keeps filters when navigating back from a workload', async () => {
    const { r } = await mountAt('/workloads?namespace=b&sort=-name')
    await r.push('/workloads/b/Deployment/web')
    await flushPromises()
    r.back()
    await flushPromises()
    await new Promise((resolve) => setTimeout(resolve, 0))
    expect(r.currentRoute.value.fullPath).toBe('/workloads?namespace=b&sort=-name')
  })

  it('toggles sort direction through the query string', async () => {
    ;(api.api as any).mockResolvedValue(oneRow)
    const { w, r } = await mountAt('/workloads')
    await header(w, 'Name').trigger('click')
    await flushPromises()
    expect(r.currentRoute.value.query.sort).toBe('-name')
    expect(header(w, 'Name').text()).toContain('\u25BC')
    await header(w, 'Kind').trigger('click')
    await flushPromises()
    expect(r.currentRoute.value.query.sort).toBe('kind')
    expect(lastApiParams().get('sort')).toBe('kind')
  })

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
          stalePods: 2,
          totalPods: 5,
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
    expect(w.text()).toContain('2/5')
  })

  it('marks inactive rows with a badge and last-seen', async () => {
    ;(api.api as any).mockResolvedValue({
      items: [
        {
          namespace: 'airflow',
          kind: 'Pod',
          name: 'etl',
          containers: [],
          automated: true,
          policyName: 'p',
          riskState: 'safe',
          stalePods: 0,
          totalPods: 3,
          autoscalerPresent: false,
          active: false,
          lastSeenAt: new Date(Date.now() - 2 * 3600 * 1000).toISOString(),
        },
      ],
      total: 1,
      pageSize: 50,
      namespaces: ['airflow'],
      kinds: ['Pod'],
      counts: { total: 1, automated: 1, manual: 0 },
    })
    const w = mount(WorkloadsView, { global: { plugins: [router] } })
    await flushPromises()
    expect(w.text()).toContain('Inactive')
    expect(w.text()).toContain('last seen')
  })

  it('resets every filter but keeps the sort order', async () => {
    const { w, r } = await mountAt(
      '/workloads?namespace=a&kind=Deployment&automated=true&active=false&risk=safe&autoscaler=no-autoscaler&search=web&page=2&sort=-kind',
    )
    const reset = w.find('button[data-test="reset-filters"]')
    expect(reset.attributes('disabled')).toBeUndefined()
    await reset.trigger('click')
    await flushPromises()
    expect(r.currentRoute.value.query).toEqual({ sort: '-kind' })
    expect(
      (w.find('input[type="text"][placeholder^="Search"]').element as HTMLInputElement).value,
    ).toBe('')
    expect(w.find('button[data-test="reset-filters"]').attributes('disabled')).toBeDefined()
  })

  it('enables reset while a search is still debouncing', async () => {
    vi.useFakeTimers()
    try {
      const { w, r } = await mountAt('/workloads')
      expect(w.find('button[data-test="reset-filters"]').attributes('disabled')).toBeDefined()
      await w.find('input[type="text"][placeholder^="Search"]').setValue('api')
      await w.find('button[data-test="reset-filters"]').trigger('click')
      vi.advanceTimersByTime(300)
      await flushPromises()
      expect(r.currentRoute.value.query.search).toBeUndefined()
    } finally {
      vi.useRealTimers()
    }
  })
  it('renders the name and policy as real links', async () => {
    ;(api.api as any).mockResolvedValue({
      ...oneRow,
      items: [{ ...oneRow.items[0], policyName: 'p' }],
    })
    const { w } = await mountAt('/workloads')
    expect(w.find('a.row-link').attributes('href')).toBe('/workloads/a/Deployment/web')
    expect(w.find('td[data-label="Policy"] a').attributes('href')).toBe('/policies/p')
  })

  it('opens the workload in a new tab on Cmd-click or middle-click of the row', async () => {
    ;(api.api as any).mockResolvedValue(oneRow)
    const open = vi.spyOn(window, 'open').mockReturnValue(null)
    try {
      const { w, r } = await mountAt('/workloads?namespace=a')
      const row = w.find('tbody tr')
      await row.trigger('click', { metaKey: true })
      await row.trigger('auxclick', { button: 1 })
      await flushPromises()
      expect(open).toHaveBeenCalledTimes(2)
      expect(open).toHaveBeenCalledWith('/workloads/a/Deployment/web', '_blank')
      expect(r.currentRoute.value.fullPath).toBe('/workloads?namespace=a')
    } finally {
      open.mockRestore()
    }
  })

  it('does not double-handle a click on the name link', async () => {
    ;(api.api as any).mockResolvedValue(oneRow)
    const open = vi.spyOn(window, 'open').mockReturnValue(null)
    try {
      const { w, r } = await mountAt('/workloads')
      const push = vi.spyOn(r, 'push')
      await w.find('a.row-link').trigger('click')
      await flushPromises()
      expect(push).toHaveBeenCalledTimes(1)
      expect(r.currentRoute.value.path).toBe('/workloads/a/Deployment/web')
      push.mockClear()
      await w.find('a.row-link').trigger('auxclick', { button: 1 })
      expect(open).not.toHaveBeenCalled()
    } finally {
      open.mockRestore()
    }
  })
  it('restarts a new search from page 1', async () => {
    vi.useFakeTimers()
    try {
      const { w, r } = await mountAt('/workloads?page=3')
      expect(lastApiParams().get('page')).toBe('3')
      await w.find('input[type="text"][placeholder^="Search"]').setValue('web')
      vi.advanceTimersByTime(300)
      await flushPromises()
      expect(r.currentRoute.value.query).toEqual({ search: 'web' })
      expect(lastApiParams().get('page')).toBe('1')
      expect(lastApiParams().get('search')).toBe('web')
    } finally {
      vi.useRealTimers()
    }
  })

  it('snaps back to the last page when the URL points past the end', async () => {
    ;(api.api as any).mockImplementation(async (path: string) => {
      const page = new URLSearchParams(path.split('?')[1]).get('page')
      return page === '2' ? { ...oneRow, total: 51 } : { ...emptyList, total: 51 }
    })
    const { w, r } = await mountAt('/workloads?search=web&page=7')
    await flushPromises()
    expect(r.currentRoute.value.query).toEqual({ search: 'web', page: '2' })
    expect(w.text()).not.toContain('No matches')
    expect(w.find('a.row-link').exists()).toBe(true)
  })
})
