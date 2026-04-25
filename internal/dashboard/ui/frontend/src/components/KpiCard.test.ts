import { mount } from '@vue/test-utils'
import { describe, it, expect } from 'vitest'
import KpiCard from './KpiCard.vue'

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
})
