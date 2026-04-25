import { describe, it, expect, vi } from 'vitest'
import { useApi } from './useApi'

describe('useApi', () => {
  it('exposes loading/data/error refs', async () => {
    const fetcher = vi.fn().mockResolvedValue({ x: 1 })
    const { data, loading, error, run } = useApi(fetcher)
    expect(loading.value).toBe(false)
    const p = run()
    expect(loading.value).toBe(true)
    await p
    expect(loading.value).toBe(false)
    expect(data.value).toEqual({ x: 1 })
    expect(error.value).toBe('')
  })

  it('cancels stale results when re-run', async () => {
    const fetcher = vi
      .fn()
      .mockImplementationOnce(() => new Promise((r) => setTimeout(() => r({ id: 'old' }), 50)))
      .mockImplementationOnce(() => Promise.resolve({ id: 'new' }))
    const { data, run } = useApi(fetcher)
    const stale = run()
    await run()
    await stale
    expect(data.value).toEqual({ id: 'new' })
  })
})
