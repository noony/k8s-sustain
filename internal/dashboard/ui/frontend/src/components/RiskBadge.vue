<script setup lang="ts">
defineProps<{ state?: 'safe' | 'drifted' | 'at-risk' | 'blocked' | string }>()
const labels: Record<string, { text: string; cls: string }> = {
  safe: { text: 'Safe', cls: 'risk-safe' },
  drifted: { text: 'Drift', cls: 'risk-drift' },
  'at-risk': { text: 'At risk', cls: 'risk-risk' },
  blocked: { text: 'Blocked', cls: 'risk-blocked' },
}
const fallback = { text: '-', cls: 'risk-unknown' }
</script>

<template>
  <span class="risk-badge" :class="(labels[state ?? ''] ?? fallback).cls">{{
    (labels[state ?? ''] ?? fallback).text
  }}</span>
</template>
