import { describe, it, expect, vi, beforeEach } from 'vitest'
import { updateModes, type WorkloadKind } from './kinds'

vi.mock('./api', () => ({ api: vi.fn() }))

const kinds: WorkloadKind[] = [
  { name: 'Deployment', updateField: 'deployment', inPlaceOnly: false },
  { name: 'Rollout', updateField: 'argoRollout', inPlaceOnly: false },
  { name: 'CronJob', updateField: 'cronJob', inPlaceOnly: true },
  { name: 'Pod', updateField: 'pod', inPlaceOnly: true },
]

describe('getKinds', () => {
  beforeEach(() => {
    vi.resetModules()
  })

  async function freshModule() {
    const api = (await import('./api')).api as unknown as ReturnType<typeof vi.fn>
    api.mockReset()
    const { getKinds } = await import('./kinds')
    return { api, getKinds }
  }

  it('fetches the kinds once for every caller', async () => {
    const { api, getKinds } = await freshModule()
    api.mockResolvedValue(kinds)

    const [first, second] = await Promise.all([getKinds(), getKinds()])
    const third = await getKinds()

    expect(api).toHaveBeenCalledTimes(1)
    expect(api).toHaveBeenCalledWith('/api/kinds')
    expect(first).toEqual(kinds)
    expect(second).toBe(first)
    expect(third).toBe(first)
  })

  it('retries after a failed fetch instead of keeping the failure', async () => {
    const { api, getKinds } = await freshModule()
    api.mockRejectedValueOnce(new Error('dashboard restarting')).mockResolvedValue(kinds)

    await expect(getKinds()).rejects.toThrow('dashboard restarting')
    await expect(getKinds()).resolves.toEqual(kinds)
    await getKinds()

    expect(api).toHaveBeenCalledTimes(2)
  })
})

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

  it('names each managed kind by its raw key without the kinds', () => {
    expect(updateModes(null, { argoRollout: 'Ongoing', cronJob: 'OnCreate' })).toBe(
      'argoRollout: Ongoing, cronJob: OnCreate',
    )
  })

  it('shows a dash for a Policy managing no kind', () => {
    expect(updateModes(kinds, undefined)).toBe('-')
    expect(updateModes(kinds, {})).toBe('-')
    expect(updateModes([], { deployment: 'Ongoing' })).toBe('-')
    expect(updateModes(null, undefined)).toBe('-')
  })
})
