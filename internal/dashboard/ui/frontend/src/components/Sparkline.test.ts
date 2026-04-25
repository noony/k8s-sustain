import { mount } from '@vue/test-utils'
import { describe, it, expect } from 'vitest'
import Sparkline from './Sparkline.vue'

describe('Sparkline', () => {
  it('renders an SVG path with the right number of points', () => {
    const wrapper = mount(Sparkline, { props: { points: [1, 2, 3, 4], color: '#3fb950' } })
    const path = wrapper.find('path').attributes('d') || ''
    expect(path.split(/[ML]/).length).toBeGreaterThan(2)
  })
  it('renders empty placeholder when no points', () => {
    const wrapper = mount(Sparkline, { props: { points: [], color: '#3fb950' } })
    expect(wrapper.find('path').exists()).toBe(false)
  })
})
