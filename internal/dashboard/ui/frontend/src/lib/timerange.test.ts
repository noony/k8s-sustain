import { describe, it, expect } from 'vitest'
import {
  PRESETS,
  DEFAULT_RANGE,
  presetToWindow,
  resolveRange,
  stepForRange,
  encodeRange,
  decodeRange,
  rangeQueryParams,
  simulateRangeBody,
} from './timerange'

const NOW = 1_718_000_000_000 // ms

describe('presets', () => {
  it('has the nine Datadog presets', () => {
    expect(PRESETS.map((p) => p.window)).toEqual([
      '5m',
      '15m',
      '30m',
      '1h',
      '4h',
      '1d',
      '2d',
      '1w',
      '1mo',
    ])
  })
})

describe('presetToWindow', () => {
  it('maps 1mo to a backend-valid 30d', () => {
    expect(presetToWindow('1mo')).toBe('30d')
  })
  it('passes through valid windows', () => {
    expect(presetToWindow('1w')).toBe('1w')
    expect(presetToWindow('15m')).toBe('15m')
  })
})

describe('resolveRange', () => {
  it('anchors a relative range to now', () => {
    expect(resolveRange({ kind: 'relative', window: '1h' }, NOW)).toEqual({
      fromTs: NOW / 1000 - 3600,
      toTs: NOW / 1000,
    })
  })
  it('returns an absolute range verbatim', () => {
    expect(resolveRange({ kind: 'absolute', fromTs: 100, toTs: 200 }, NOW)).toEqual({
      fromTs: 100,
      toTs: 200,
    })
  })
})

describe('stepForRange', () => {
  it('picks finer steps for short spans and coarser for long', () => {
    expect(stepForRange(30 * 60 * 1000)).toBe('15s')
    expect(stepForRange(24 * 60 * 60 * 1000)).toBe('5m')
    expect(stepForRange(30 * 24 * 60 * 60 * 1000)).toBe('2h')
  })
})

describe('encode/decode round-trip', () => {
  it('relative writes from_ts/to_ts plus window hint', () => {
    const enc = encodeRange({ kind: 'relative', window: '1h' }, NOW)
    expect(enc.window).toBe('1h')
    expect(enc.from_ts).toBe(String(NOW / 1000 - 3600))
    expect(enc.to_ts).toBe(String(NOW / 1000))
    expect(decodeRange(new URLSearchParams(enc))).toEqual({ kind: 'relative', window: '1h' })
  })
  it('absolute writes only from_ts/to_ts', () => {
    const enc = encodeRange({ kind: 'absolute', fromTs: 100, toTs: 200 }, NOW)
    expect(enc.window).toBeUndefined()
    expect(decodeRange(new URLSearchParams(enc))).toEqual({
      kind: 'absolute',
      fromTs: 100,
      toTs: 200,
    })
  })
  it('falls back to DEFAULT_RANGE when empty', () => {
    expect(decodeRange(new URLSearchParams())).toEqual(DEFAULT_RANGE)
  })
})

describe('simulateRangeBody', () => {
  it('relative sends window + step, no fromTs/toTs', () => {
    const body = simulateRangeBody({ kind: 'relative', window: '1mo' }, NOW)
    expect(body.window).toBe('30d')
    expect(body.step).toBe(stepForRange(30 * 24 * 60 * 60 * 1000))
    expect(body.fromTs).toBeUndefined()
    expect(body.toTs).toBeUndefined()
  })
  it('absolute sends fromTs/toTs + step, no window', () => {
    const fromTs = NOW / 1000 - 3600
    const toTs = NOW / 1000
    const body = simulateRangeBody({ kind: 'absolute', fromTs, toTs }, NOW)
    expect(body.fromTs).toBe(fromTs)
    expect(body.toTs).toBe(toTs)
    expect(body.step).toBe(stepForRange((toTs - fromTs) * 1000))
    expect(body.window).toBeUndefined()
  })
  it('clamps a future toTs down to now', () => {
    const fromTs = NOW / 1000 - 3600
    const toTs = NOW / 1000 + 6 * 3600 // 6h in the future
    const body = simulateRangeBody({ kind: 'absolute', fromTs, toTs }, NOW)
    expect(body.toTs).toBe(NOW / 1000)
  })
})

describe('rangeQueryParams', () => {
  it('relative sends a backend window + step', () => {
    const p = rangeQueryParams({ kind: 'relative', window: '1mo' }, NOW)
    expect(p.window).toBe('30d')
    expect(p.from).toBeUndefined()
    expect(p.step).toBe(stepForRange(30 * 24 * 60 * 60 * 1000))
  })
  it('absolute sends from/to + step', () => {
    const p = rangeQueryParams(
      { kind: 'absolute', fromTs: NOW / 1000 - 3600, toTs: NOW / 1000 },
      NOW,
    )
    expect(p.from).toBe(String(NOW / 1000 - 3600))
    expect(p.to).toBe(String(NOW / 1000))
    expect(p.step).toBe('1m')
  })
  it('clamps a future to down to now', () => {
    const p = rangeQueryParams(
      { kind: 'absolute', fromTs: NOW / 1000 - 3600, toTs: NOW / 1000 + 6 * 3600 },
      NOW,
    )
    expect(p.to).toBe(String(NOW / 1000))
  })
})
