// src/lib/chart.test.ts
import { describe, it, expect } from 'vitest'
import { zoomedRangeSeconds } from './chart'
import type { Chart } from 'chart.js'

// zoomedRangeSeconds only reads chart.scales.x.{min,max} (epoch ms), so a
// minimal stub is enough — no real canvas/Chart.js rendering required.
function chartWithX(min: unknown, max: unknown): Chart {
  return { scales: { x: { min, max } } } as unknown as Chart
}

describe('zoomedRangeSeconds', () => {
  it('converts the x-scale window (ms) to epoch seconds', () => {
    const r = zoomedRangeSeconds(chartWithX(1781906400000, 1782079140000))
    expect(r).toEqual({ fromTs: 1781906400, toTs: 1782079140 })
  })

  it('floors the start and ceils the end so the window is not clipped', () => {
    const r = zoomedRangeSeconds(chartWithX(1000500, 2000500))
    expect(r).toEqual({ fromTs: 1000, toTs: 2001 })
  })

  it('returns null when min >= max', () => {
    expect(zoomedRangeSeconds(chartWithX(2000, 2000))).toBeNull()
    expect(zoomedRangeSeconds(chartWithX(3000, 2000))).toBeNull()
  })

  it('returns null when bounds are not finite', () => {
    expect(zoomedRangeSeconds(chartWithX(NaN, 2000))).toBeNull()
    expect(zoomedRangeSeconds(chartWithX(undefined, undefined))).toBeNull()
  })
})
