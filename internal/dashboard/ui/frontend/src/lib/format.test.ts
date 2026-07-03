import { describe, expect, it } from 'vitest'
import { timeAgo } from './format'

function isoAgo(ms: number): string {
  return new Date(Date.now() - ms).toISOString()
}

describe('timeAgo', () => {
  it('returns "-" for missing input', () => {
    expect(timeAgo()).toBe('-')
    expect(timeAgo('')).toBe('-')
  })

  it('returns "-" for unparseable input', () => {
    expect(timeAgo('not-a-date')).toBe('-')
  })

  it('renders sub-hour ages with minute precision (date-fns strict)', () => {
    expect(timeAgo(isoAgo(5 * 60_000))).toBe('5 minutes ago')
  })

  it('renders hour ages', () => {
    expect(timeAgo(isoAgo(3 * 3_600_000))).toBe('3 hours ago')
  })

  it('renders day ages', () => {
    expect(timeAgo(isoAgo(2 * 24 * 3_600_000))).toBe('2 days ago')
  })
})
