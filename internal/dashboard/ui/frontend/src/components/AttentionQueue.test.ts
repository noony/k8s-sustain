import { mount } from '@vue/test-utils'
import { describe, it, expect } from 'vitest'
import { createRouter, createMemoryHistory } from 'vue-router'
import AttentionQueue from './AttentionQueue.vue'

function testRouter() {
  return createRouter({
    history: createMemoryHistory(),
    routes: [{ path: '/:pathMatch(.*)*', component: { template: '<div />' } }],
  })
}

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
  it('renders rows as links to the workload', () => {
    const w = mount(AttentionQueue, {
      props: {
        groups: {
          risk: [{ namespace: 'a', kind: 'Deployment', name: 'web', signal: 'OOM' }],
          drift: [],
          blocked: [],
        },
      },
      global: { plugins: [testRouter()] },
    })
    const row = w.find('a.aq-row')
    expect(row.exists()).toBe(true)
    expect(row.attributes('href')).toBe('/workloads/a/Deployment/web')
  })
})
