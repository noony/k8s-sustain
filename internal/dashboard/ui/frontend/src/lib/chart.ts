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
    ctx.strokeStyle = 'rgba(228,230,237,0.35)'
    ctx.lineWidth = 1
    ctx.moveTo(x, yScale.top)
    ctx.lineTo(x, yScale.bottom)
    ctx.stroke()
    ctx.restore()
  },
}
Chart.register(crosshairPlugin)

// OOM event plugin
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

    events.forEach((ev: { x: Date }) => {
      const x = xScale.getPixelForValue(ev.x as any)
      if (x < xScale.left || x > xScale.right) return

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
      ctx.strokeStyle = '#1a1d27'
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
  const chartData = points.map((p) => ({ x: new Date(p.timestamp) as any, y: transform(p.value) }))

  const datasets: any[] = [
    {
      label: opts.label,
      data: chartData,
      borderColor: opts.color,
      backgroundColor: opts.color + '18',
      fill: true,
      borderWidth: 1.5,
      pointRadius: 0,
      pointHoverRadius: 4,
      tension: 0.3,
    },
  ]

  // Extra time-series
  ;(opts.extraSeries || []).forEach((s) => {
    if (!s.data || !s.data.length) return
    const seriesData = s.data.map((p) => ({
      x: new Date(p.timestamp) as any,
      y: transform(p.value),
    }))
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
          labels: { color: '#8b8fa3', font: { size: 11 } },
        },
        tooltip: {
          backgroundColor: '#1a1d27',
          borderColor: '#2a2e3f',
          borderWidth: 1,
          titleColor: '#e4e6ed',
          bodyColor: '#e4e6ed',
          callbacks: {
            label: (ctx: any) =>
              ctx.dataset.label + ': ' + opts.yFormat(ctx.parsed.y) + ' ' + opts.unit,
          },
        },
        zoom: {
          zoom: {
            drag: {
              enabled: true,
              backgroundColor: 'rgba(99,102,241,0.25)',
              borderColor: 'rgba(99,102,241,0.6)',
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
          grid: { color: '#2a2e3f' },
          ticks: { color: '#8b8fa3', font: { size: 11 }, maxTicksLimit: 8 },
        },
        y: {
          grid: { color: '#2a2e3f' },
          ticks: {
            color: '#8b8fa3',
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
