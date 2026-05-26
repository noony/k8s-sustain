import {
  Chart,
  LineController,
  LineElement,
  PointElement,
  LinearScale,
  TimeScale,
  Filler,
  Legend,
  Tooltip,
  type Plugin,
  type ChartConfiguration,
} from 'chart.js'
import 'chartjs-adapter-date-fns'
import zoomPlugin from 'chartjs-plugin-zoom'
import type { TimeValue } from './api'

// Register Chart.js components
Chart.register(
  LineController,
  LineElement,
  PointElement,
  LinearScale,
  TimeScale,
  Filler,
  Legend,
  Tooltip,
  zoomPlugin,
)

// Crosshair plugin (Grafana-style vertical line following mouse)
const crosshairPlugin: Plugin = {
  id: 'crosshair',
  afterEvent(chart, args) {
    const evt = args.event
    if (evt.type === 'mouseout') {
      ;(chart as any)._crosshairX = null
      chart.draw()
      return
    }
    if (evt.type === 'mousemove') {
      ;(chart as any)._crosshairX = evt.x
      chart.draw()
    }
  },
  afterDatasetsDraw(chart) {
    const x = (chart as any)._crosshairX
    if (x == null) return
    const yScale = chart.scales.y
    const xScale = chart.scales.x
    if (x < xScale.left || x > xScale.right) return
    const ctx = chart.ctx
    ctx.save()
    ctx.beginPath()
    ctx.setLineDash([3, 3])
    ctx.strokeStyle = themeVar('--chart-crosshair', 'rgba(228,230,237,0.35)')
    ctx.lineWidth = 1
    ctx.moveTo(x, yScale.top)
    ctx.lineTo(x, yScale.bottom)
    ctx.stroke()
    ctx.restore()
  },
}
Chart.register(crosshairPlugin)

function themeVar(name: string, fallback: string): string {
  if (typeof window === 'undefined' || typeof document === 'undefined') return fallback
  try {
    const v = getComputedStyle(document.documentElement).getPropertyValue(name).trim()
    return v || fallback
  } catch {
    return fallback
  }
}

// Produce a soft fill color from any chart line color (hex, rgb, hsl).
// Falls back to a low-alpha overlay of the source color.
function softFill(color: string, alpha = 0.12): string {
  const c = color.trim()
  // #RGB / #RRGGBB → rgba()
  const hex = c.match(/^#([0-9a-f]{3}|[0-9a-f]{6})$/i)
  if (hex) {
    let h = hex[1]
    if (h.length === 3)
      h = h
        .split('')
        .map((x) => x + x)
        .join('')
    const r = parseInt(h.slice(0, 2), 16)
    const g = parseInt(h.slice(2, 4), 16)
    const b = parseInt(h.slice(4, 6), 16)
    return `rgba(${r}, ${g}, ${b}, ${alpha})`
  }
  // rgb(r, g, b) → rgba(r, g, b, a)
  const rgb = c.match(/^rgb\(\s*(\d+)\s*,\s*(\d+)\s*,\s*(\d+)\s*\)$/i)
  if (rgb) return `rgba(${rgb[1]}, ${rgb[2]}, ${rgb[3]}, ${alpha})`
  // rgba(...) → keep components, override alpha
  const rgba = c.match(/^rgba\(\s*(\d+)\s*,\s*(\d+)\s*,\s*(\d+)\s*,\s*[^)]+\)$/i)
  if (rgba) return `rgba(${rgba[1]}, ${rgba[2]}, ${rgba[3]}, ${alpha})`
  // Unknown format — return transparent so it doesn't render as black.
  return 'rgba(0, 0, 0, 0)'
}

function themeColors() {
  return {
    grid: themeVar('--chart-grid', 'rgba(255,255,255,0.06)'),
    tick: themeVar('--chart-tick', '#8b8fa3'),
    tooltipBg: themeVar('--chart-tooltip-bg', '#161c27'),
    tooltipBorder: themeVar('--chart-tooltip-border', '#2a2e3f'),
    text: themeVar('--text', '#e6edf3'),
    accent: themeVar('--accent', '#7c3aed'),
    accentSoft: themeVar('--accent-soft', 'rgba(124,58,237,0.25)'),
  }
}

// OOM event plugin
const OOM_MARKER_LIMIT = 50
const oomEventPlugin: Plugin = {
  id: 'oomEvents',
  afterDraw(chart) {
    const events = (chart.options.plugins as any)?.oomEvents
    if (!events || events.length === 0) return
    const ctx = chart.ctx
    const xScale = chart.scales.x
    const yScale = chart.scales.y
    const top = yScale.top
    const bottom = yScale.bottom

    // Defensive cap: if dedup at the API layer fails for some reason, stop
    // drawing markers past the limit to keep the chart legible. The badge in
    // the UI shows the true count.
    const limited = events.length > OOM_MARKER_LIMIT ? events.slice(0, OOM_MARKER_LIMIT) : events

    limited.forEach((ev: { x: Date }) => {
      const x = xScale.getPixelForValue(ev.x as any)
      if (!Number.isFinite(x) || x < xScale.left || x > xScale.right) return

      ctx.save()
      ctx.beginPath()
      ctx.setLineDash([4, 4])
      ctx.strokeStyle = 'rgba(239, 68, 68, 0.6)'
      ctx.lineWidth = 1
      ctx.moveTo(x, top)
      ctx.lineTo(x, bottom)
      ctx.stroke()
      ctx.setLineDash([])

      ctx.beginPath()
      ctx.arc(x, top + 10, 5, 0, 2 * Math.PI)
      ctx.fillStyle = '#ef4444'
      ctx.fill()
      ctx.strokeStyle = themeVar('--bg-card', '#1a1d27')
      ctx.lineWidth = 1.5
      ctx.stroke()

      ctx.fillStyle = '#fff'
      ctx.font = 'bold 7px sans-serif'
      ctx.textAlign = 'center'
      ctx.textBaseline = 'middle'
      ctx.fillText('\u2715', x, top + 10)
      ctx.restore()
    })
  },
}
Chart.register(oomEventPlugin)

export interface ExtraSeries {
  data: TimeValue[]
  label: string
  color?: string
  dash?: number[]
  stepped?: boolean | 'before'
}

export interface ChartAnnotation {
  value: number
  label: string
  color?: string
  dash?: number[]
}

export interface ChartOpts {
  label: string
  color: string
  unit: string
  transform?: (v: number) => number
  yFormat: (v: number) => string
  annotations?: ChartAnnotation[]
  extraSeries?: ExtraSeries[]
  oomEvents?: { timestamp: string; pod?: string }[]
  onZoomComplete?: (chart: Chart) => void
  // When set, breaks the line at gaps wider than ~1.5× this step. Used to
  // avoid drawing a continuous line across periods where the workload had no
  // pods running (e.g. between CronJob runs).
  stepMs?: number
}

type ChartPoint = { x: Date; y: number | null }

// withGaps converts a sparse time-series into chart data, inserting an
// explicit null between consecutive points spaced farther apart than
// gapThreshold so Chart.js breaks the line instead of connecting across the
// gap. When stepMs is omitted, points are returned as-is.
function withGaps(
  points: TimeValue[] | undefined,
  transform: (v: number) => number,
  stepMs?: number,
): ChartPoint[] {
  if (!points || !points.length) return []
  const threshold = stepMs ? stepMs * 1.5 : 0
  const out: ChartPoint[] = []
  let prevT: number | null = null
  for (const p of points) {
    const t = new Date(p.timestamp).getTime()
    if (threshold > 0 && prevT !== null && t - prevT > threshold) {
      out.push({ x: new Date(prevT + 1), y: null })
    }
    out.push({ x: new Date(t), y: transform(p.value) })
    prevT = t
  }
  return out
}

// Global chart instance registry
const chartInstances: Record<string, Chart> = {}

export function getChartInstance(id: string): Chart | undefined {
  return chartInstances[id]
}

export function destroyAllCharts() {
  Object.values(chartInstances).forEach((c) => c.destroy())
  for (const key in chartInstances) delete chartInstances[key]
}

export function destroyChart(id: string) {
  if (chartInstances[id]) {
    chartInstances[id].destroy()
    delete chartInstances[id]
  }
}

export function createTimeSeriesChart(
  canvasId: string,
  points: TimeValue[],
  opts: ChartOpts,
): Chart | null {
  const canvas = document.getElementById(canvasId) as HTMLCanvasElement | null
  if (!canvas) return null

  // Clean up previous instance
  destroyChart(canvasId)

  const transform = opts.transform || ((v: number) => v)
  const chartData = withGaps(points, transform, opts.stepMs)

  const datasets: any[] = [
    {
      label: opts.label,
      data: chartData,
      borderColor: opts.color,
      backgroundColor: (ctx: any) => {
        const chart = ctx.chart
        const area = chart.chartArea
        if (!area) return softFill(opts.color, 0.12)
        const grad = chart.ctx.createLinearGradient(0, area.top, 0, area.bottom)
        grad.addColorStop(0, softFill(opts.color, 0.32))
        grad.addColorStop(1, softFill(opts.color, 0))
        return grad
      },
      fill: true,
      borderWidth: 1.75,
      pointRadius: 0,
      pointHoverRadius: 4,
      pointHoverBorderWidth: 2,
      pointHoverBackgroundColor: opts.color,
      pointHoverBorderColor: themeVar('--bg-card', '#0d1117'),
      tension: 0.3,
    },
  ]

  // Extra time-series
  ;(opts.extraSeries || []).forEach((s) => {
    if (!s.data || !s.data.length) return
    const seriesData = withGaps(s.data, transform, opts.stepMs)
    const ds: any = {
      label: s.label,
      data: seriesData,
      borderColor: s.color || '#f59e0b',
      borderWidth: 1.5,
      borderDash: s.dash || [4, 4],
      pointRadius: 0,
      fill: false,
    }
    if (s.stepped !== false) ds.stepped = 'before'
    datasets.push(ds)
  })

  // Annotation lines (flat horizontal)
  const annotations = [...(opts.annotations || [])]
  annotations.forEach((anno) => {
    if (anno.value == null || isNaN(anno.value)) return
    const val = transform(anno.value)
    datasets.push({
      label: anno.label,
      data: chartData.map((p) => ({ x: p.x, y: val })),
      borderColor: anno.color || '#ef4444',
      borderWidth: 1.5,
      borderDash: anno.dash || [8, 4],
      pointRadius: 0,
      fill: false,
    })
  })

  // OOM markers
  const oomMarkers = (opts.oomEvents || []).map((ev) => ({
    x: new Date(ev.timestamp),
    pod: ev.pod || '',
  }))

  const colors = themeColors()
  const config: ChartConfiguration = {
    type: 'line',
    data: { datasets },
    options: {
      responsive: true,
      maintainAspectRatio: false,
      interaction: { mode: 'index', intersect: false },
      plugins: {
        legend: {
          display: datasets.length > 1,
          labels: { color: colors.tick, font: { size: 11 } },
        },
        tooltip: {
          backgroundColor: colors.tooltipBg,
          borderColor: colors.tooltipBorder,
          borderWidth: 1,
          titleColor: colors.text,
          bodyColor: colors.text,
          padding: 10,
          cornerRadius: 6,
          boxPadding: 6,
          usePointStyle: true,
          callbacks: {
            label: (ctx: any) =>
              ctx.dataset.label + ': ' + opts.yFormat(ctx.parsed.y) + ' ' + opts.unit,
          },
        },
        zoom: {
          zoom: {
            drag: {
              enabled: true,
              backgroundColor: colors.accentSoft,
              borderColor: colors.accent,
              borderWidth: 1,
              threshold: 5,
            },
            mode: 'x',
            onZoomComplete: (ctx: any) => {
              if (opts.onZoomComplete) opts.onZoomComplete(ctx.chart)
            },
          },
        } as any,
        oomEvents: oomMarkers,
      } as any,
      scales: {
        x: {
          type: 'time',
          grid: { color: colors.grid },
          ticks: { color: colors.tick, font: { size: 11 }, maxTicksLimit: 8 },
        },
        y: {
          grid: { color: colors.grid },
          ticks: {
            color: colors.tick,
            font: { size: 11 },
            callback: (v: any) => opts.yFormat(v),
          },
          beginAtZero: true,
        },
      },
    },
  }

  const chart = new Chart(canvas, config)
  chartInstances[canvasId] = chart

  // Double-click to reset zoom
  canvas.addEventListener('dblclick', () => resetZoom(canvasId))

  return chart
}

// Re-apply theme tokens to every live chart instance. Called when the user
// toggles the theme so charts don't stay frozen with the old palette.
export function applyThemeToAllCharts() {
  const colors = themeColors()
  Object.values(chartInstances).forEach((chart) => {
    const opts: any = chart.options
    if (opts.scales?.x) {
      opts.scales.x.grid = { ...opts.scales.x.grid, color: colors.grid }
      opts.scales.x.ticks = { ...opts.scales.x.ticks, color: colors.tick }
    }
    if (opts.scales?.y) {
      opts.scales.y.grid = { ...opts.scales.y.grid, color: colors.grid }
      opts.scales.y.ticks = { ...opts.scales.y.ticks, color: colors.tick }
    }
    const plugins = opts.plugins || {}
    if (plugins.tooltip) {
      plugins.tooltip.backgroundColor = colors.tooltipBg
      plugins.tooltip.borderColor = colors.tooltipBorder
      plugins.tooltip.titleColor = colors.text
      plugins.tooltip.bodyColor = colors.text
    }
    if (plugins.legend?.labels) {
      plugins.legend.labels.color = colors.tick
    }
    if (plugins.zoom?.zoom?.drag) {
      plugins.zoom.zoom.drag.backgroundColor = colors.accentSoft
      plugins.zoom.zoom.drag.borderColor = colors.accent
    }
    // The primary dataset uses a hover border in the card background color —
    // recompute it via the scriptable function on next draw by forcing update.
    chart.data.datasets.forEach((ds: any) => {
      if (ds.pointHoverBorderColor) ds.pointHoverBorderColor = themeVar('--bg-card', '#0d1117')
    })
    chart.update('none')
  })
}

if (typeof window !== 'undefined') {
  window.addEventListener('themechange', () => applyThemeToAllCharts())
}

export function pairedCanvasId(canvasId: string): string | null {
  if (canvasId.startsWith('simcpu-')) return 'simmem-' + canvasId.slice(7)
  if (canvasId.startsWith('simmem-')) return 'simcpu-' + canvasId.slice(7)
  if (canvasId.startsWith('cpu-')) return 'mem-' + canvasId.slice(4)
  if (canvasId.startsWith('mem-')) return 'cpu-' + canvasId.slice(4)
  return null
}

export function showResetZoomBtn(canvasId: string, show: boolean) {
  const btn = document.getElementById('rz-' + canvasId)
  if (btn) btn.style.display = show ? 'block' : 'none'
}

export function syncZoom(sourceChart: Chart) {
  const id = sourceChart.canvas.id
  const pairId = pairedCanvasId(id)
  if (!pairId || !chartInstances[pairId]) return
  const pair = chartInstances[pairId]
  const srcScale = sourceChart.scales.x
  ;(pair as any).zoomScale('x', { min: srcScale.min, max: srcScale.max }, 'none')
  showResetZoomBtn(id, true)
  showResetZoomBtn(pairId, true)
}

export function resetZoom(canvasId: string) {
  const chart = chartInstances[canvasId]
  if (chart) {
    ;(chart as any).resetZoom()
    showResetZoomBtn(canvasId, false)
  }
  const pairId = pairedCanvasId(canvasId)
  if (pairId && chartInstances[pairId]) {
    ;(chartInstances[pairId] as any).resetZoom()
    showResetZoomBtn(pairId, false)
  }
}

export function groupOOMEventsByContainer(
  events?: { container: string; timestamp: string; pod: string }[],
): Record<string, { timestamp: string; pod: string }[]> {
  const byContainer: Record<string, { timestamp: string; pod: string }[]> = {}
  if (!events || !events.length) return byContainer
  events.forEach((ev) => {
    if (!byContainer[ev.container]) byContainer[ev.container] = []
    byContainer[ev.container].push(ev)
  })
  return byContainer
}
