import { mount } from '@vue/test-utils'
import EventList from './EventList.vue'
import { describe, it, expect } from 'vitest'

describe('EventList', () => {
  it('renders rows', () => {
    const w = mount(EventList, {
      props: {
        items: [
          { timestamp: 't', namespace: 'n', kind: 'K', name: 'x', reason: 'R', message: 'm' },
        ],
      },
    })
    expect(w.findAll('.event-row').length).toBe(1)
    expect(w.text()).toContain('R')
    expect(w.text()).toContain('m')
  })
})
