import { mount } from '@vue/test-utils'
import { describe, it, expect } from 'vitest'
import AttentionQueue from './AttentionQueue.vue'

describe('AttentionQueue', () => {
  it('renders three groups with counts', () => {
    const w = mount(AttentionQueue, {
      props: {
        groups: {
          risk: [{ namespace: 'a', kind: 'Deployment', name: 'web', signal: 'OOM' }],
          drift: [
            { namespace: 'a', kind: 'Deployment', name: 'api', signal: 'drift', detail: '18%' },
          ],
          blocked: [],
        },
      },
    })
    expect(w.text()).toContain('Risk')
    expect(w.text()).toContain('Drift')
    expect(w.text()).toContain('Blocked')
    expect(w.text()).toContain('1') // count
    expect(w.findAll('.aq-row').length).toBe(2)
  })
})
