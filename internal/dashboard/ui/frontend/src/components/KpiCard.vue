<script setup lang="ts">
import Sparkline from './Sparkline.vue'

defineProps<{
  label: string
  value: string
  detail?: string
  tone?: 'positive' | 'negative' | 'neutral' | 'warn' | 'danger'
  sparkPoints?: number[]
  sparkColor?: string
  clickable?: boolean
}>()
defineEmits<{
  (e: 'click'): void
}>()
</script>

<template>
  <component
    :is="clickable ? 'button' : 'div'"
    class="kpi-card"
    :class="[tone ? 'tone-' + tone : '', clickable ? 'kpi-clickable' : '']"
    :type="clickable ? 'button' : undefined"
    @click="clickable ? $emit('click') : null"
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
