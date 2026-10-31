import type { ContainerResources, RecommendationContainer, Resource, TimeValue } from './api'
import type { ChartAnnotation, ExtraSeries, SeriesKey } from './chart'
import { parseCPUQuantity, parseMemoryQuantity } from './format'

/** How the detail page and the Simulator chart one resource. */
export interface ChartResource {
  key: Resource
  /** The resource as a heading names it. */
  label: string
  /** The resource as a sentence names it. */
  noun: string
  color: SeriesKey
  /** The prefix of its charts' canvas ids. */
  canvas: string
  unit: string
  transform?: (v: number) => number
  yFormat: (v: number) => string
  parse: (q: string) => number
  request: 'cpuRequest' | 'memoryRequest'
  limit: 'cpuLimit' | 'memoryLimit'
  /** Its charts mark the OOM kills. */
  marksOOMKills: boolean
}

export const chartResources: readonly ChartResource[] = [
  {
    key: 'cpu',
    label: 'CPU',
    noun: 'CPU',
    color: 'cpu',
    canvas: 'cpu',
    unit: 'cores',
    yFormat: (v) => v.toFixed(3),
    parse: parseCPUQuantity,
    request: 'cpuRequest',
    limit: 'cpuLimit',
    marksOOMKills: false,
  },
  {
    key: 'memory',
    label: 'Memory',
    noun: 'memory',
    color: 'mem',
    canvas: 'mem',
    unit: 'MiB',
    transform: (v) => v / (1024 * 1024),
    yFormat: (v) => v.toFixed(0),
    parse: parseMemoryQuantity,
    request: 'memoryRequest',
    limit: 'memoryLimit',
    marksOOMKills: true,
  },
]

interface SeriesStyle {
  label: string
  color: SeriesKey
  dash: number[]
}

/** The lines both views draw, each always in the same style and under the same label. */
export const seriesStyles = {
  runningRequest: { label: 'Request', color: 'config', dash: [4, 4] },
  runningLimit: { label: 'Limit', color: 'config', dash: [1, 4] },
  recommendedRequest: { label: 'Recommended request', color: 'rec', dash: [8, 4] },
  recommendedLimit: { label: 'Recommended limit', color: 'rec', dash: [2, 3] },
} satisfies Record<string, SeriesStyle>

function flat(style: SeriesStyle, value: number, shown: string): ChartAnnotation {
  return { value, label: `${style.label}: ${shown}`, color: style.color, dash: style.dash }
}

/**
 * What the pods ran with: the series Prometheus holds, or, without one, the
 * value the spec declares as a flat line.
 */
export function runningLines(
  r: ChartResource,
  requests: TimeValue[] | undefined,
  limits: TimeValue[] | undefined,
  spec: ContainerResources,
): { extra: ExtraSeries[]; annotations: ChartAnnotation[] } {
  const extra: ExtraSeries[] = []
  const annotations: ChartAnnotation[] = []
  for (const [data, declared, style] of [
    [requests, spec[r.request], seriesStyles.runningRequest],
    [limits, spec[r.limit], seriesStyles.runningLimit],
  ] as const) {
    if (data?.length) extra.push({ data, ...style })
    else if (declared) annotations.push(flat(style, r.parse(declared), declared))
  }
  return { extra, annotations }
}

/** A recommendation computed at each instant, which moves between instants rather than in steps. */
export function recommendedSeries(
  requests: TimeValue[] | undefined,
  limits: TimeValue[] | undefined,
): ExtraSeries[] {
  const out: ExtraSeries[] = []
  if (requests?.length)
    out.push({ data: requests, ...seriesStyles.recommendedRequest, stepped: false })
  if (limits?.length) out.push({ data: limits, ...seriesStyles.recommendedLimit, stepped: false })
  return out
}

/** A stored Recommendation as flat lines: it is not recomputed at past instants. */
export function storedLines(
  r: ChartResource,
  rec: RecommendationContainer | undefined,
): ChartAnnotation[] {
  const out: ChartAnnotation[] = []
  const request = rec?.[r.request]
  const limit = rec?.[r.limit]
  if (request) out.push(flat(seriesStyles.recommendedRequest, r.parse(request), request))
  if (limit) out.push(flat(seriesStyles.recommendedLimit, r.parse(limit), limit))
  return out
}
