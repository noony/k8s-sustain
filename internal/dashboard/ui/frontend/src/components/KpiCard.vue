<script setup lang="ts">
import { RouterLink, type RouteLocationRaw } from 'vue-router'
import Sparkline from './Sparkline.vue'

defineProps<{
  label: string
  value: string
  detail?: string
  tone?: 'positive' | 'negative' | 'neutral' | 'warn' | 'danger'
  sparkPoints?: number[]
  sparkColor?: string
  to?: RouteLocationRaw
}>()
</script>

<template>
  <component
    :is="to ? RouterLink : 'div'"
    class="kpi-card"
    :class="[tone ? 'tone-' + tone : '', to ? 'kpi-clickable' : '']"
    :to="to"
  >
    <div class="kpi-label">{{ label }}</div>
    <div class="kpi-row">
      <div class="kpi-value">{{ value }}</div>
      <Sparkline
        v-if="sparkPoints && sparkPoints.length"
        :points="sparkPoints"
        :color="sparkColor || 'rgb(124, 58, 237)'"
      />
    </div>
    <div v-if="detail" class="kpi-detail">{{ detail }}</div>
  </component>
</template>
