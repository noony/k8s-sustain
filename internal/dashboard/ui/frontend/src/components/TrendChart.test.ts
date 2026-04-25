import { mount } from '@vue/test-utils'
import { describe, it, expect } from 'vitest'
import TrendChart from './TrendChart.vue'

describe('TrendChart', () => {
  it('mounts and exposes a canvas', () => {
    const w = mount(TrendChart, {
      props: {
        series: [
          {
            label: 'CPU',
            color: '#3fb950',
            points: [{ timestamp: '2026-04-01T00:00:00Z', value: 1 }],
          },
        ],
        unit: 'cores',
      },
    })
    expect(w.find('canvas').exists()).toBe(true)
  })
})
