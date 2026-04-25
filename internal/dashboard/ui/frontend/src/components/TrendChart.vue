<script setup lang="ts">
import { onMounted, onBeforeUnmount, watch } from 'vue'
import { createTimeSeriesChart, destroyAllCharts, type ExtraSeries } from '../lib/chart'
import type { TimeValue } from '../lib/api'

const props = defineProps<{
  series: { label: string; color: string; points: TimeValue[] }[]
  unit: string
  height?: number
  yFormat?: (v: number) => string
}>()

const id = 'trend-' + Math.random().toString(36).slice(2)
const wrapHeight = props.height ?? 240

function render() {
  if (!props.series.length) return
  const [primary, ...rest] = props.series
  const extra: ExtraSeries[] = rest.map((s) => ({
    data: s.points,
    label: s.label,
    color: s.color,
    dash: [],
  }))
  try {
    createTimeSeriesChart(id, primary.points, {
      label: primary.label,
      color: primary.color,
      unit: props.unit,
      yFormat: props.yFormat ?? ((v) => v.toFixed(2)),
      extraSeries: extra,
      annotations: [],
    })
  } catch {
    // jsdom lacks canvas getContext('2d'); swallow so component still mounts in tests
  }
}

onMounted(render)
onBeforeUnmount(destroyAllCharts)
watch(
  () => props.series,
  () => {
    destroyAllCharts()
    render()
  },
  { deep: true },
)
</script>

<template>
  <div class="trend-chart" :style="{ height: wrapHeight + 'px' }">
    <canvas :id="id"></canvas>
  </div>
</template>
