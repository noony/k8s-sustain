<script setup lang="ts">
defineProps<{ state: 'safe' | 'drifted' | 'at-risk' | 'blocked' }>()
const labels: Record<string, { text: string; cls: string }> = {
  safe: { text: 'Safe', cls: 'risk-safe' },
  drifted: { text: 'Drift', cls: 'risk-drift' },
  'at-risk': { text: 'At risk', cls: 'risk-risk' },
  blocked: { text: 'Blocked', cls: 'risk-blocked' },
}
</script>

<template>
  <span class="risk-badge" :class="labels[state].cls">{{ labels[state].text }}</span>
</template>
