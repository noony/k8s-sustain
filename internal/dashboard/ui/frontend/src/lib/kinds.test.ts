import { describe, it, expect } from 'vitest'
import { updateModes, type WorkloadKind } from './kinds'

const kinds: WorkloadKind[] = [
  { name: 'Deployment', updateField: 'deployment', inPlaceOnly: false },
  { name: 'Rollout', updateField: 'argoRollout', inPlaceOnly: false },
  { name: 'CronJob', updateField: 'cronJob', inPlaceOnly: true },
  { name: 'Pod', updateField: 'pod', inPlaceOnly: true },
]

describe('updateModes', () => {
  it("names each managed kind by the server's field for it, in the server's order", () => {
    expect(
      updateModes(kinds, { pod: 'OnCreate', argoRollout: 'Ongoing', deployment: 'OnCreate' }),
    ).toBe('Deployment: OnCreate, Rollout: Ongoing, Pod: OnCreate')
  })

  it('marks Ongoing on a kind whose pods are only resized in place', () => {
    expect(updateModes(kinds, { cronJob: 'Ongoing', deployment: 'Ongoing' })).toBe(
      'Deployment: Ongoing, CronJob: Ongoing (in place)',
    )
  })

  it('shows a dash for a Policy managing no kind', () => {
    expect(updateModes(kinds, undefined)).toBe('-')
    expect(updateModes(kinds, {})).toBe('-')
    expect(updateModes([], { deployment: 'Ongoing' })).toBe('-')
  })
})
