import { mount } from '@vue/test-utils'
import { describe, it, expect } from 'vitest'
import RecommendationTrace from './RecommendationTrace.vue'
import type { ContainerTrace } from '../lib/api'

function cells(trace: ContainerTrace): Record<string, string[]> {
  const w = mount(RecommendationTrace, { props: { trace } })
  const out: Record<string, string[]> = {}
  for (const row of w.findAll('tbody tr')) {
    const [stage, ...values] = row.findAll('td').map((td) => td.text())
    out[stage] = values
  }
  return out
}

describe('RecommendationTrace', () => {
  it('shows each stage per resource in one unit, with the coordination factors', () => {
    const got = cells({
      cpu: {
        percentile: '123400u',
        withHeadroom: '136m',
        clamped: '150m',
        coordination: { overheadFactor: 1.375, replicaFactor: 2, scaled: '414m', value: '414m' },
        limit: '414m',
      },
      memory: {
        percentile: '104857600',
        withHeadroom: '110Mi',
        clamped: '110Mi',
        coordination: { overheadFactor: 1, scaled: '110Mi', value: '110Mi' },
      },
    })

    expect(got['Usage percentile']).toEqual(['123.4m', '100Mi'])
    expect(got['With headroom']).toEqual(['136m', '110Mi'])
    expect(got['Min/max clamp']).toEqual(['150m', '110Mi'])
    expect(got['Autoscaler coordination']).toEqual([
      '×1.38 overhead, ×2 replica → 414m',
      '×1 overhead → 110Mi',
    ])
    expect(got['Limit']).toEqual(['414m', 'kept'])
    expect(got['OOM floor']).toBeUndefined()
  })

  it('says when the OOM floor set the request and when a clamp replaced the coordinated value', () => {
    const got = cells({
      memory: {
        oomFloor: { value: '240Mi', determined: false },
        withHeadroom: '240Mi',
        clamped: '240Mi',
        coordination: { overheadFactor: 1.375, scaled: '330Mi', value: '256Mi' },
        removeLimit: true,
      },
    })

    expect(got['Usage percentile']).toBeUndefined()
    expect(got['OOM floor']).toEqual(['–', '240Mi'])
    expect(got['Autoscaler coordination']).toEqual([
      '–',
      '×1.38 overhead → 330Mi, clamped to 256Mi',
    ])
    expect(got['Limit']).toEqual(['–', 'removed'])

    const determined = cells({
      memory: {
        oomFloor: { value: '240Mi', determined: true },
        withHeadroom: '240Mi',
        clamped: '240Mi',
      },
    })
    expect(determined['OOM floor']).toEqual(['–', '240Mi · set the request'])
    expect(determined['Autoscaler coordination']).toBeUndefined()
  })

  it('renders nothing for an empty trace', () => {
    const w = mount(RecommendationTrace, { props: { trace: {} } })
    expect(w.find('[data-test="trace"]').exists()).toBe(false)
  })
})
