<script setup lang="ts">
const props = defineProps<{ points: number[]; color: string; width?: number; height?: number }>()
const w = props.width ?? 80
const h = props.height ?? 22

function path(): string {
  if (!props.points.length) return ''
  const min = Math.min(...props.points)
  const max = Math.max(...props.points)
  const span = max - min || 1
  const stepX = w / Math.max(props.points.length - 1, 1)
  return props.points
    .map((v, i) => {
      const x = (i * stepX).toFixed(1)
      const y = (h - ((v - min) / span) * h).toFixed(1)
      return `${i === 0 ? 'M' : 'L'}${x},${y}`
    })
    .join(' ')
}
</script>

<template>
  <svg :viewBox="`0 0 ${w} ${h}`" :width="w" :height="h">
    <path
      v-if="points.length"
      :d="path()"
      :stroke="color"
      stroke-width="1.2"
      fill="none"
      stroke-linejoin="round"
    />
  </svg>
</template>
