import { mount } from '@vue/test-utils'
import { describe, it, expect } from 'vitest'
import RiskBadge from './RiskBadge.vue'

describe('RiskBadge', () => {
  it.each([
    ['safe', 'Safe'],
    ['drifted', 'Drifted'],
    ['at-risk', 'At risk'],
    ['blocked', 'Blocked'],
    ['conflicted', 'Conflicted'],
  ])('names %s as the glossary does', (state, label) => {
    const wrapper = mount(RiskBadge, { props: { state } })
    expect(wrapper.text()).toBe(label)
    expect(wrapper.classes()).toContain(`risk-${state}`)
  })

  it('shows a dash for a state it does not know', () => {
    const wrapper = mount(RiskBadge, { props: { state: 'unheard-of' } })
    expect(wrapper.text()).toBe('-')
  })
})
