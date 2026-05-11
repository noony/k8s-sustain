<script setup lang="ts">
defineProps<{
  variant?: 'kpi' | 'table' | 'simple'
  message?: string
  rows?: number
}>()
</script>

<template>
  <template v-if="variant === 'kpi'">
    <div class="skeleton-kpi" aria-busy="true" :aria-label="message || 'Loading'">
      <div v-for="i in 4" :key="i" class="skeleton-kpi-card">
        <div class="skeleton" style="height: 11px; width: 60%"></div>
        <div class="skeleton" style="height: 22px; width: 80%"></div>
        <div class="skeleton" style="height: 11px; width: 40%"></div>
      </div>
    </div>
    <div class="card">
      <div class="skeleton skeleton-block"></div>
    </div>
  </template>
  <template v-else-if="variant === 'table'">
    <div class="card" aria-busy="true" :aria-label="message || 'Loading'">
      <div
        v-for="i in rows || 6"
        :key="i"
        class="skeleton-row"
        :style="{ opacity: 1 - (i - 1) * 0.1 }"
      >
        <div class="skeleton skeleton-text"></div>
        <div class="skeleton skeleton-text"></div>
        <div class="skeleton skeleton-text"></div>
        <div class="skeleton skeleton-text"></div>
        <div class="skeleton skeleton-text"></div>
      </div>
    </div>
  </template>
  <template v-else>
    <div class="state-card" role="status" :aria-label="message || 'Loading'">
      <div class="spinner"></div>
      <div class="state-message">{{ message || 'Loading…' }}</div>
    </div>
  </template>
</template>
