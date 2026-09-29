import { mount } from '@vue/test-utils'
import { describe, it, expect } from 'vitest'
import { createRouter, createMemoryHistory } from 'vue-router'
import KpiCard from './KpiCard.vue'

function testRouter() {
  return createRouter({
    history: createMemoryHistory(),
    routes: [{ path: '/:pathMatch(.*)*', component: { template: '<div />' } }],
  })
}

describe('KpiCard', () => {
  it('renders label, value, and detail', () => {
    const w = mount(KpiCard, {
      props: { label: 'CPU saved', value: '3.2c', detail: '28% vs last week', tone: 'positive' },
    })
    expect(w.text()).toContain('CPU saved')
    expect(w.text()).toContain('3.2c')
    expect(w.text()).toContain('28% vs last week')
    expect(w.classes().some((c) => c.includes('positive'))).toBe(true)
  })
  it('renders sparkline when points provided', () => {
    const w = mount(KpiCard, { props: { label: 'x', value: '1', sparkPoints: [1, 2, 3] } })
    expect(w.findComponent({ name: 'Sparkline' }).exists()).toBe(true)
  })
  it('renders a real link when given a target, so Cmd-click opens a new tab', () => {
    const w = mount(KpiCard, {
      props: { label: 'At risk', value: '3', to: '/workloads?risk=at-risk' },
      global: { plugins: [testRouter()] },
    })
    expect(w.element.tagName).toBe('A')
    expect(w.attributes('href')).toBe('/workloads?risk=at-risk')
    expect(w.classes()).toContain('kpi-clickable')
  })
  it('renders a plain block without a target', () => {
    const w = mount(KpiCard, { props: { label: 'x', value: '1' } })
    expect(w.element.tagName).toBe('DIV')
    expect(w.classes()).not.toContain('kpi-clickable')
  })
})
