<script setup lang="ts">
defineProps<{
  title?: string
  message?: string
  retryLabel?: string
}>()
defineEmits<{
  (e: 'retry'): void
}>()
</script>

<template>
  <div class="state-card" role="alert">
    <div class="state-icon state-icon-error">
      <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2">
        <circle cx="12" cy="12" r="10" />
        <path d="M12 8v4M12 16h.01" stroke-linecap="round" />
      </svg>
    </div>
    <div class="state-title">{{ title || 'Something went wrong' }}</div>
    <div class="state-message">
      {{ message || 'An unexpected error occurred while loading this data.' }}
    </div>
    <div class="state-actions">
      <button class="btn btn-primary btn-sm" type="button" @click="$emit('retry')">
        {{ retryLabel || 'Retry' }}
      </button>
    </div>
  </div>
</template>
