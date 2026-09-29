import { mount, flushPromises, type VueWrapper } from '@vue/test-utils'
import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import PolicyDetailView from './PolicyDetailView.vue'
import * as api from '../lib/api'
import { createRouter, createMemoryHistory, createWebHistory, type Router } from 'vue-router'

vi.mock('../lib/api', async (o) => ({ ...((await o()) as object), api: vi.fn() }))

describe('PolicyDetailView', () => {
  it('renders effectiveness chart band and view-as-yaml button', async () => {
    ;(api.api as any).mockImplementation((path: string) => {
      if (path === '/api/policies/p')
        return Promise.resolve({
          name: 'p',
          conditions: [],
          spec: { rightSizing: { resourcesConfigs: { cpu: {}, memory: {} } } },
          effectivenessSeries: { cpu: [], memory: [] },
        })
      if (path.startsWith('/api/policies/p/workloads'))
        return Promise.resolve({ items: [], total: 0, pageSize: 50 })
      return Promise.resolve({})
    })
    const router = createRouter({
      history: createMemoryHistory(),
      routes: [{ path: '/:catchAll(.*)', component: { template: '<div/>' } }],
    })
    const w = mount(PolicyDetailView, {
      props: { name: 'p' },
      global: { plugins: [router], stubs: ['router-link', 'StatusBadge', 'TrendChart'] },
    })
    await flushPromises()
    expect(w.text()).toContain('Effectiveness')
    expect(w.text()).toContain('View as YAML')
  })

  describe('matched workloads list', () => {
    const policyBody = {
      name: 'p',
      conditions: [],
      spec: { rightSizing: { resourcesConfigs: { cpu: {}, memory: {} } } },
      effectivenessSeries: { cpu: [], memory: [] },
    }
    const row = { namespace: 'a', kind: 'Deployment', name: 'web', containers: [] }
    let workloadsResponse: (params: URLSearchParams) => unknown

    function apiCalls(prefix: string): URLSearchParams[] {
      return (api.api as any).mock.calls
        .map((c: unknown[]) => c[0] as string)
        .filter((p: string) => p.startsWith(prefix))
        .map((p: string) => new URLSearchParams(p.split('?')[1]))
    }
    const workloadCalls = () => apiCalls('/api/policies/p/workloads?')
    const lastWorkloadParams = () => workloadCalls().at(-1)!

    beforeEach(() => {
      workloadsResponse = () => ({
        items: [row],
        total: 1,
        matched: 7,
        pageSize: 50,
        namespaces: ['a', 'b'],
      })
      ;(api.api as any).mockReset()
      ;(api.api as any).mockImplementation(async (path: string) => {
        if (path.startsWith('/api/policies/p/workloads?'))
          return workloadsResponse(new URLSearchParams(path.split('?')[1]))
        if (path.startsWith('/api/policies/p')) return policyBody
        return {}
      })
    })
    afterEach(() => window.history.replaceState(null, '', '/'))

    async function mountAt(url: string, router?: Router) {
      const r =
        router ??
        createRouter({
          history: createMemoryHistory(),
          routes: [{ path: '/:catchAll(.*)', component: { template: '<div/>' } }],
        })
      await r.push(url)
      await r.isReady()
      const w = mount(PolicyDetailView, {
        props: { name: 'p' },
        global: { plugins: [r], stubs: ['StatusBadge', 'TrendChart'] },
      })
      await flushPromises()
      return { w, r }
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

    it('sorts by name by default', async () => {
      const { w, r } = await mountAt('/policies/p')
      expect(lastWorkloadParams().get('sort')).toBe('name')
      expect(r.currentRoute.value.query.sort).toBeUndefined()
      expect(header(w, 'Name').text()).toContain('\u25B2')
    })

    it('restores namespace, search, page and sort from the query string', async () => {
      const { w } = await mountAt('/policies/p?namespace=b&search=web&page=2&sort=-stalePods')
      const p = lastWorkloadParams()
      expect(p.get('namespace')).toBe('b')
      expect(p.get('search')).toBe('web')
      expect(p.get('page')).toBe('2')
      expect(p.get('sort')).toBe('-stalePods')
      const input = w.find('input[type="text"][placeholder^="Search"]').element as HTMLInputElement
      expect(input.value).toBe('web')
    })

    it('toggles sort through the query string without refetching the policy', async () => {
      const { w, r } = await mountAt('/policies/p')
      const policyFetches = () => apiCalls('/api/policies/p?').length
      const before = policyFetches()
      await header(w, 'Name').trigger('click')
      await flushPromises()
      expect(r.currentRoute.value.query.sort).toBe('-name')
      expect(lastWorkloadParams().get('sort')).toBe('-name')
      await header(w, 'Drift').trigger('click')
      await flushPromises()
      expect(r.currentRoute.value.query.sort).toBe('stalePods')
      expect(policyFetches()).toBe(before)
    })

    it('debounces search into the query string and restarts from page 1', async () => {
      vi.useFakeTimers()
      try {
        const { w, r } = await mountAt('/policies/p?page=3')
        await w.find('input[type="text"][placeholder^="Search"]').setValue('api')
        vi.advanceTimersByTime(300)
        await flushPromises()
        expect(r.currentRoute.value.query).toEqual({ search: 'api' })
        expect(lastWorkloadParams().get('page')).toBe('1')
      } finally {
        vi.useRealTimers()
      }
    })

    it('resets filters but keeps the sort order', async () => {
      const { w, r } = await mountAt('/policies/p?namespace=a&search=web&page=2&sort=kind')
      const reset = w.find('button[data-test="reset-filters"]')
      expect(reset.attributes('disabled')).toBeUndefined()
      await reset.trigger('click')
      await flushPromises()
      expect(r.currentRoute.value.query).toEqual({ sort: 'kind' })
      expect(w.find('button[data-test="reset-filters"]').attributes('disabled')).toBeDefined()
    })

    it('shows the unfiltered count on the Matched Workloads card', async () => {
      const { w } = await mountAt('/policies/p?search=web')
      const card = w.findAll('.stat-card').find((c) => c.text().includes('Matched Workloads'))!
      expect(card.find('.stat-value').text()).toBe('7')
    })

    it('snaps back to the last page when the URL points past the end', async () => {
      workloadsResponse = (params) =>
        params.get('page') === '2'
          ? { items: [row], total: 51, matched: 51, pageSize: 50, namespaces: ['a'] }
          : { items: [], total: 51, matched: 51, pageSize: 50, namespaces: ['a'] }
      const { w, r } = await mountAt('/policies/p?page=9')
      await flushPromises()
      expect(r.currentRoute.value.query).toEqual({ page: '2' })
      expect(w.find('a.row-link').exists()).toBe(true)
    })

    it('keeps the time range in the URL when a list filter changes', async () => {
      const router = createRouter({
        history: createWebHistory(),
        routes: [{ path: '/:catchAll(.*)', component: { template: '<div/>' } }],
      })
      const { w } = await mountAt('/policies/p', router)
      // What useTimeRange does on a range change: rewrite the URL behind the router.
      window.history.replaceState(window.history.state, '', '/policies/p?window=6h')
      await header(w, 'Kind').trigger('click')
      await flushPromises()
      const params = new URLSearchParams(window.location.search)
      expect(params.get('window')).toBe('6h')
      expect(params.get('sort')).toBe('kind')
    })
  })
})
