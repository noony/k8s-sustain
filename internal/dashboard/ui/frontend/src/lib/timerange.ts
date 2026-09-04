export interface Preset {
  window: string
  badge: string
  label: string
}

export const PRESETS: Preset[] = [
  { window: '5m', badge: '5m', label: 'Past 5 Minutes' },
  { window: '15m', badge: '15m', label: 'Past 15 Minutes' },
  { window: '30m', badge: '30m', label: 'Past 30 Minutes' },
  { window: '1h', badge: '1h', label: 'Past 1 Hour' },
  { window: '4h', badge: '4h', label: 'Past 4 Hours' },
  { window: '1d', badge: '1d', label: 'Past 1 Day' },
  { window: '2d', badge: '2d', label: 'Past 2 Days' },
  { window: '1w', badge: '1w', label: 'Past 1 Week' },
  { window: '1mo', badge: '1mo', label: 'Past 1 Month' },
]

export type TimeRange =
  { kind: 'relative'; window: string } | { kind: 'absolute'; fromTs: number; toTs: number }

export const DEFAULT_RANGE: TimeRange = { kind: 'relative', window: '1w' }

const PRESET_WINDOWS = new Set(PRESETS.map((p) => p.window))

const WINDOW_MS: Record<string, number> = {
  '5m': 5 * 60_000,
  '15m': 15 * 60_000,
  '30m': 30 * 60_000,
  '1h': 60 * 60_000,
  '4h': 4 * 60 * 60_000,
  '1d': 24 * 60 * 60_000,
  '2d': 2 * 24 * 60 * 60_000,
  '1w': 7 * 24 * 60 * 60_000,
  '1mo': 30 * 24 * 60 * 60_000,
}

// Map a Datadog-style preset id to a backend-valid Prometheus window.
// Only '1mo' is invalid backend syntax (no 'mo' unit); the rest pass through.
export function presetToWindow(window: string): string {
  return window === '1mo' ? '30d' : window
}

export function resolveRange(r: TimeRange, nowMs: number): { fromTs: number; toTs: number } {
  if (r.kind === 'absolute') return { fromTs: r.fromTs, toTs: r.toTs }
  const toTs = Math.floor(nowMs / 1000)
  return { fromTs: toTs - WINDOW_MS[r.window] / 1000, toTs }
}

// Pick a Prometheus step string targeting a few hundred points across the span.
export function stepForRange(durationMs: number): string {
  const m = 60_000,
    h = 60 * m,
    d = 24 * h
  if (durationMs <= 30 * m) return '15s'
  if (durationMs < h) return '30s'
  if (durationMs <= 6 * h) return '1m'
  if (durationMs <= d) return '5m'
  if (durationMs <= 3 * d) return '10m'
  if (durationMs <= 7 * d) return '20m'
  if (durationMs <= 14 * d) return '1h'
  return '2h'
}

function spanMs(r: TimeRange): number {
  if (r.kind === 'relative') return WINDOW_MS[r.window]
  return (r.toTs - r.fromTs) * 1000
}

export function encodeRange(r: TimeRange, nowMs: number): Record<string, string> {
  const { fromTs, toTs } = resolveRange(r, nowMs)
  const out: Record<string, string> = { from_ts: String(fromTs), to_ts: String(toTs) }
  if (r.kind === 'relative') out.window = r.window
  return out
}

export function decodeRange(p: URLSearchParams): TimeRange {
  const window = p.get('window')
  if (window && PRESET_WINDOWS.has(window)) return { kind: 'relative', window }
  const from = p.get('from_ts')
  const to = p.get('to_ts')
  if (from && to) {
    const fromTs = Number(from)
    const toTs = Number(to)
    if (Number.isFinite(fromTs) && Number.isFinite(toTs) && fromTs < toTs) {
      return { kind: 'absolute', fromTs, toTs }
    }
  }
  return DEFAULT_RANGE
}

// Clamp an absolute end timestamp (epoch seconds) to "now". There is no data
// ahead of now, and the backend rejects to > now+1h — so a stale bookmarked URL
// or a range ending today must not send a future `to`.
function clampToNow(toTs: number, nowMs: number): number {
  return Math.min(toTs, Math.floor(nowMs / 1000))
}

// Body fields for POST /api/simulate. Relative ranges send `window` (+ step);
// absolute ranges send fromTs/toTs (epoch seconds) (+ step). Mirrors
// rangeQueryParams but with the simulate body's key names.
export function simulateRangeBody(
  r: TimeRange,
  nowMs: number,
): { window?: string; fromTs?: number; toTs?: number; step: string } {
  if (r.kind === 'relative')
    return { window: presetToWindow(r.window), step: stepForRange(spanMs(r)) }
  const toTs = clampToNow(r.toTs, nowMs)
  return { fromTs: r.fromTs, toTs, step: stepForRange((toTs - r.fromTs) * 1000) }
}

export function rangeQueryParams(r: TimeRange, nowMs: number): Record<string, string> {
  if (r.kind === 'relative')
    return { window: presetToWindow(r.window), step: stepForRange(spanMs(r)) }
  const toTs = clampToNow(r.toTs, nowMs)
  return { from: String(r.fromTs), to: String(toTs), step: stepForRange((toTs - r.fromTs) * 1000) }
}
