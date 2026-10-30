import { mount } from '@vue/test-utils'
import { describe, it, expect } from 'vitest'
import { createRouter, createMemoryHistory } from 'vue-router'
import AttentionQueue from './AttentionQueue.vue'
import type { AttentionGroup, AttentionRow } from '../lib/api'

function testRouter() {
  return createRouter({
    history: createMemoryHistory(),
    routes: [{ path: '/:pathMatch(.*)*', component: { template: '<div />' } }],
  })
}

const row = (name: string, signal: string): AttentionRow => ({
  namespace: 'a',
  kind: 'Deployment',
  name,
  signal,
})

function groups(overrides: Partial<Record<string, AttentionGroup>> = {}) {
  const empty = { total: 0, rows: [] }
  return {
    conflicted: empty,
    blocked: empty,
    'at-risk': empty,
    drifted: empty,
    ...overrides,
  }
}

describe('AttentionQueue', () => {
  it('renders the four Risk-state groups in precedence order', () => {
    const w = mount(AttentionQueue, {
      props: {
        groups: groups({
          'at-risk': { total: 1, rows: [row('web', 'OOM')] },
          drifted: { total: 1, rows: [row('api', 'drift')] },
        }),
      },
      global: { plugins: [testRouter()] },
    })
    expect(w.findAll('.aq-title').map((t) => t.text())).toEqual([
      'Conflicted',
      'Blocked',
      'At risk',
      'Drifted',
    ])
    expect(w.findAll('.aq-row').length).toBe(2)
  })

  it('shows the backend total, not the listed rows, and links to the rest', () => {
    const w = mount(AttentionQueue, {
      props: { groups: groups({ blocked: { total: 37, rows: [row('web', 'blocked')] } }) },
      global: { plugins: [testRouter()] },
    })
    expect(w.findAll('.aq-count').map((c) => c.text())).toEqual(['0', '37', '0', '0'])
    const more = w.find('a.aq-more')
    expect(more.text()).toBe('View all 37')
    expect(more.attributes('href')).toBe('/workloads?risk=blocked')
  })

  it('renders rows as links to the workload', () => {
    const w = mount(AttentionQueue, {
      props: { groups: groups({ 'at-risk': { total: 1, rows: [row('web', 'OOM')] } }) },
      global: { plugins: [testRouter()] },
    })
    const link = w.find('a.aq-row')
    expect(link.exists()).toBe(true)
    expect(link.attributes('href')).toBe('/workloads/a/Deployment/web')
    expect(w.find('a.aq-more').exists()).toBe(false)
  })

  it('collapses and reopens a group from its header', async () => {
    const w = mount(AttentionQueue, {
      props: { groups: groups({ 'at-risk': { total: 1, rows: [row('web', 'OOM')] } }) },
      global: { plugins: [testRouter()] },
    })
    const header = w.findAll('.aq-header')[2]
    await header.trigger('click')
    expect(w.find('a.aq-row').exists()).toBe(false)
    await header.trigger('click')
    expect(w.find('a.aq-row').exists()).toBe(true)
  })
})
