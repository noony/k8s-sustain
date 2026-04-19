<script setup lang="ts">
import { parseCPUQuantity, parseMemoryQuantity } from '../lib/format'

const props = defineProps<{
  current?: string
  recommended?: string
  resourceType: 'cpu' | 'memory'
}>()

function delta(): number | null {
  if (!props.current || !props.recommended) return null
  const parser = props.resourceType === 'cpu' ? parseCPUQuantity : parseMemoryQuantity
  const curVal = parser(props.current)
  const recVal = parser(props.recommended)
  if (isNaN(curVal) || isNaN(recVal) || curVal <= 0) return null
  return Math.round(((recVal - curVal) / curVal) * 1000) / 10
}

function diffClass(): string {
  const d = delta()
  if (d == null) return 'neutral'
  return d < -5 ? 'saving' : d > 5 ? 'increase' : 'neutral'
}
</script>

<template>
  <span v-if="!recommended" style="color: var(--text-dim)">-</span>
  <span v-else-if="!current" class="recommended">{{ recommended }}</span>
  <template v-else>
    <span class="resource-diff">
      <span class="old">{{ current }}</span>
      <span class="arrow">&rarr;</span>
      <span class="new" :class="diffClass()">{{ recommended }}</span>
    </span>
    <span v-if="delta() != null && delta() !== 0" class="delta-badge" :class="diffClass()">
      {{ delta()! < 0 ? '&#8595;' : '&#8593;' }}
      {{ Math.abs(delta()!).toFixed(1) }}%
    </span>
  </template>
</template>
