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

function stages(trace: ContainerTrace): string[] {
  const w = mount(RecommendationTrace, { props: { trace } })
  return w.findAll('tbody tr').map((row) => row.attributes('data-stage') ?? '')
}

describe('RecommendationTrace', () => {
  it('shows each stage per resource in one unit, with its facts', () => {
    const got = cells({
      cpu: {
        request: [
          { stage: 'percentile', value: '123400u' },
          { stage: 'headroom', value: '136m' },
          { stage: 'clamp', value: '150m' },
          {
            stage: 'coordination',
            value: '414m',
            facts: { replicaFactor: '2', overheadFactor: '1.375' },
          },
        ],
        limit: [{ stage: 'equalsToRequest', value: '414m' }],
      },
      memory: {
        request: [
          { stage: 'percentile', value: '104857600' },
          { stage: 'headroom', value: '110Mi' },
          { stage: 'clamp', value: '110Mi' },
        ],
        limit: [{ stage: 'keepLimit' }],
      },
    })

    expect(got['Usage percentile']).toEqual(['123.4m', '100Mi'])
    expect(got['With headroom']).toEqual(['136m', '110Mi'])
    expect(got['Min/max clamp']).toEqual(['150m', '110Mi'])
    expect(got['Autoscaler coordination']).toEqual([
      '414m · overheadFactor: 1.375 · replicaFactor: 2',
      '–',
    ])
    expect(got['Limit equal to request']).toEqual(['414m', '–'])
    expect(got['Limit kept']).toEqual(['–', 'none'])
    expect(got['OOM floor']).toBeUndefined()
  })

  it('lists request stages then limit stages, each resource in its own order', () => {
    expect(
      stages({
        cpu: {
          request: [
            { stage: 'percentile', value: '100m' },
            { stage: 'headroom', value: '100m' },
            { stage: 'clamp', value: '100m' },
          ],
          limit: [{ stage: 'noLimit', removed: true }],
        },
        memory: {
          request: [
            { stage: 'percentile', value: '100Mi' },
            { stage: 'oomFloor', value: '240Mi', facts: { determined: 'true' } },
            { stage: 'headroom', value: '240Mi' },
            { stage: 'clamp', value: '240Mi' },
          ],
          limit: [
            { stage: 'keepLimit', value: '200Mi' },
            { stage: 'guard', value: '240Mi' },
          ],
        },
      }),
    ).toEqual([
      'request:percentile',
      'request:oomFloor',
      'request:headroom',
      'request:clamp',
      'limit:noLimit',
      'limit:keepLimit',
      'limit:guard',
    ])
  })

  it('renders a stage it has no name for under its own name, facts as they are', () => {
    const got = cells({
      memory: {
        request: [{ stage: 'clamp', value: '256Mi' }],
        limit: [
          { stage: 'keepLimit', value: '256Mi' },
          { stage: 'burst', value: '512Mi', facts: { factor: '2', determined: 'true' } },
          { stage: 'noLimit', removed: true },
        ],
      },
    })

    expect(got['burst']).toEqual(['–', '512Mi · determined: true · factor: 2'])
    expect(got['Limit removed']).toEqual(['–', 'removed'])
  })

  it('renders nothing for an empty trace', () => {
    const w = mount(RecommendationTrace, { props: { trace: {} } })
    expect(w.find('[data-test="trace"]').exists()).toBe(false)
  })
})
