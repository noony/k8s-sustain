import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { useAutoRefresh } from './useAutoRefresh'

describe('useAutoRefresh', () => {
  beforeEach(() => vi.useFakeTimers())
  afterEach(() => vi.useRealTimers())

  it('invokes the callback every interval', () => {
    const cb = vi.fn()
    useAutoRefresh(cb, 1000)
    vi.advanceTimersByTime(3000)
    expect(cb).toHaveBeenCalledTimes(3)
  })

  it('does not fire while the document is hidden', () => {
    const cb = vi.fn()
    Object.defineProperty(document, 'visibilityState', { value: 'hidden', configurable: true })
    useAutoRefresh(cb, 1000)
    vi.advanceTimersByTime(3000)
    expect(cb).not.toHaveBeenCalled()
    Object.defineProperty(document, 'visibilityState', { value: 'visible', configurable: true })
  })
})
