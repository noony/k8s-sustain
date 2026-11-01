import { describe, it, expect } from 'vitest'
import { limitsLabel } from './limits'

describe('limitsLabel', () => {
  it('names the default strategy when limits set none', () => {
    expect(limitsLabel(undefined)).toBe('Keep existing limit')
    expect(limitsLabel({})).toBe('Keep existing limit')
    expect(limitsLabel({ noLimit: false })).toBe('Keep existing limit')
  })

  it('follows the precedence the recommender runs strategies in', () => {
    expect(limitsLabel({ equalsToRequest: true, noLimit: true })).toBe('Remove limit')
    expect(limitsLabel({ keepLimitRequestRatio: true, requestsLimitsRatio: 2 })).toBe(
      'Multiple of request (×2)',
    )
  })
})
