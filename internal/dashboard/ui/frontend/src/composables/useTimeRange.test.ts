import { describe, it, expect, beforeEach } from 'vitest'
import { nextTick } from 'vue'
import { useTimeRange } from './useTimeRange'

describe('useTimeRange', () => {
  beforeEach(() => history.replaceState(null, '', '/'))

  it('initializes from URL params', () => {
    history.replaceState(null, '', '/?window=4h&from_ts=1&to_ts=2')
    const { range } = useTimeRange()
    expect(range.value).toEqual({ kind: 'relative', window: '4h' })
  })

  it('writes from_ts/to_ts + window to the URL on change', async () => {
    const { range } = useTimeRange()
    range.value = { kind: 'relative', window: '1h' }
    await nextTick()
    const p = new URLSearchParams(location.search)
    expect(p.get('window')).toBe('1h')
    expect(p.get('from_ts')).toBeTruthy()
    expect(p.get('to_ts')).toBeTruthy()
  })

  it('drops window for absolute ranges', async () => {
    const { range } = useTimeRange()
    range.value = { kind: 'absolute', fromTs: 100, toTs: 200 }
    await nextTick()
    const p = new URLSearchParams(location.search)
    expect(p.get('window')).toBeNull()
    expect(p.get('from_ts')).toBe('100')
  })
})
