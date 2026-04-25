import { mount } from '@vue/test-utils'
import { describe, it, expect } from 'vitest'
import RiskBadge from './RiskBadge.vue'

describe('RiskBadge', () => {
  it.each([
    ['safe', 'safe'],
    ['drifted', 'drift'],
    ['at-risk', 'risk'],
    ['blocked', 'blocked'],
  ])('renders class for %s', (state, cls) => {
    const wrapper = mount(RiskBadge, { props: { state } as any })
    expect(wrapper.classes().some((c) => c.includes(cls))).toBe(true)
  })
})
