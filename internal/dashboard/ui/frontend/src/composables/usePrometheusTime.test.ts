import { describe, it, expect } from 'vitest'
import { usePrometheusTime } from './usePrometheusTime'

describe('usePrometheusTime', () => {
  it('defaults to 168h', () => {
    const { window } = usePrometheusTime()
    expect(window.value).toBe('168h')
  })

  it('reads window from URL hash on init', () => {
    history.replaceState(null, '', '/#/overview?window=72h')
    const { window } = usePrometheusTime()
    expect(window.value).toBe('72h')
  })
})
