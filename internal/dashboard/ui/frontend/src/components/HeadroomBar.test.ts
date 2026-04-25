import { mount } from '@vue/test-utils'
import { describe, it, expect } from 'vitest'
import HeadroomBar from './HeadroomBar.vue'

describe('HeadroomBar', () => {
  it('renders three segments scaled to total', () => {
    const w = mount(HeadroomBar, { props: { used: 40, idle: 30, free: 30, label: 'CPU' } })
    const segs = w.findAll('.seg')
    expect(segs.length).toBe(3)
    const widths = segs.map((s) =>
      parseFloat(s.attributes('style')!.match(/width:\s*([\d.]+)%/)![1]),
    )
    expect(widths).toEqual([40, 30, 30])
  })
})
