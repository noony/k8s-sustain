<script setup lang="ts">
const props = defineProps<{
  used: number
  idle: number
  free: number
  label: string
  unit?: string
}>()
const total = props.used + props.idle + props.free || 1
const pct = (v: number) => (v / total) * 100
</script>

<template>
  <div class="headroom">
    <div class="headroom-label">
      {{ label }}<span v-if="unit" class="unit">({{ unit }})</span>
    </div>
    <div class="headroom-bar">
      <div class="seg seg-used" :style="{ width: pct(used) + '%' }"></div>
      <div class="seg seg-idle" :style="{ width: pct(idle) + '%' }"></div>
      <div class="seg seg-free" :style="{ width: pct(free) + '%' }"></div>
    </div>
    <div class="headroom-legend">
      <span class="dot dot-used"></span> Used <span class="dim">{{ pct(used).toFixed(0) }}%</span>
      <span class="dot dot-idle"></span> Reclaimable
      <span class="dim">{{ pct(idle).toFixed(0) }}%</span> <span class="dot dot-free"></span> Free
      <span class="dim">{{ pct(free).toFixed(0) }}%</span>
    </div>
  </div>
</template>
