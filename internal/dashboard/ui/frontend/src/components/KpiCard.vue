<script setup lang="ts">
import Sparkline from './Sparkline.vue'

defineProps<{
  label: string
  value: string
  detail?: string
  tone?: 'positive' | 'negative' | 'neutral' | 'warn' | 'danger'
  sparkPoints?: number[]
  sparkColor?: string
}>()
</script>

<template>
  <div class="kpi-card" :class="tone ? 'tone-' + tone : ''">
    <div class="kpi-label">{{ label }}</div>
    <div class="kpi-row">
      <div class="kpi-value">{{ value }}</div>
      <Sparkline
        v-if="sparkPoints && sparkPoints.length"
        :points="sparkPoints"
        :color="sparkColor || '#6366f1'"
      />
    </div>
    <div v-if="detail" class="kpi-detail">{{ detail }}</div>
  </div>
</template>
