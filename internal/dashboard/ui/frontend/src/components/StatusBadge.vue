<script setup lang="ts">
import type { Condition } from '../lib/api'

const props = defineProps<{
  conditions?: Condition[]
}>()

function statusInfo() {
  if (!props.conditions || props.conditions.length === 0)
    return { cls: 'badge-dim', text: 'Unknown' }
  const ready = props.conditions.find((c) => c.type === 'Ready')
  if (!ready) return { cls: 'badge-dim', text: 'Unknown' }
  if (ready.status === 'True') return { cls: 'badge-green', text: 'Ready' }
  return { cls: 'badge-red', text: ready.reason || 'Not Ready' }
}
</script>

<template>
  <span class="badge" :class="statusInfo().cls">{{ statusInfo().text }}</span>
</template>
