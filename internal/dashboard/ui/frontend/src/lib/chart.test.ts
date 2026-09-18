import { describe, it, expect } from 'vitest'
import { annotationPoints, xAxisBounds, zoomedRangeSeconds } from './chart'
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

describe('xAxisBounds', () => {
  it('pins the axis to the requested window (epoch seconds → ms)', () => {
    expect(xAxisBounds({ fromTs: 1789682861, toTs: 1789769261 })).toEqual({
      min: 1789682861000,
      max: 1789769261000,
    })
  })

  it('leaves the axis auto-fitted when no window is given', () => {
    expect(xAxisBounds(undefined)).toEqual({})
  })

  it('ignores an inverted or non-finite window', () => {
    expect(xAxisBounds({ fromTs: 200, toTs: 100 })).toEqual({})
    expect(xAxisBounds({ fromTs: NaN, toTs: 100 })).toEqual({})
  })
})

describe('annotationPoints', () => {
  const data = [
    { x: new Date(5000), y: 1 },
    { x: new Date(6000), y: 2 },
  ]

  it('spans the whole window so a flat line is visible with sparse data', () => {
    expect(annotationPoints(7, data, { min: 1000, max: 9000 })).toEqual([
      { x: new Date(1000), y: 7 },
      { x: new Date(9000), y: 7 },
    ])
  })

  it('follows the data extent when the axis is unbounded', () => {
    expect(annotationPoints(7, data, {})).toEqual([
      { x: new Date(5000), y: 7 },
      { x: new Date(6000), y: 7 },
    ])
  })
})
