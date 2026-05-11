<script setup lang="ts">
defineProps<{
  icon?: 'inbox' | 'search' | 'database' | 'policy' | 'chart' | 'workload'
  title?: string
  message?: string
  compact?: boolean
}>()
</script>

<template>
  <div :class="compact ? 'empty-state' : 'state-card'">
    <div v-if="!compact" class="state-icon state-icon-empty">
      <svg
        v-if="icon === 'search'"
        viewBox="0 0 24 24"
        fill="none"
        stroke="currentColor"
        stroke-width="2"
      >
        <circle cx="11" cy="11" r="7" />
        <path d="m21 21-4.3-4.3" stroke-linecap="round" />
      </svg>
      <svg
        v-else-if="icon === 'policy'"
        viewBox="0 0 24 24"
        fill="none"
        stroke="currentColor"
        stroke-width="2"
      >
        <path d="M9 5H7a2 2 0 00-2 2v12a2 2 0 002 2h10a2 2 0 002-2V7a2 2 0 00-2-2h-2" />
        <path d="M9 5a2 2 0 002 2h2a2 2 0 002-2M9 5a2 2 0 012-2h2a2 2 0 012 2" />
      </svg>
      <svg
        v-else-if="icon === 'workload'"
        viewBox="0 0 24 24"
        fill="none"
        stroke="currentColor"
        stroke-width="2"
      >
        <rect x="2" y="3" width="20" height="14" rx="2" />
        <path d="M8 21h8M12 17v4" />
      </svg>
      <svg
        v-else-if="icon === 'chart'"
        viewBox="0 0 24 24"
        fill="none"
        stroke="currentColor"
        stroke-width="2"
      >
        <path d="M3 3v18h18" stroke-linecap="round" />
        <path d="M7 14l4-4 4 4 5-5" stroke-linecap="round" stroke-linejoin="round" />
      </svg>
      <svg
        v-else-if="icon === 'database'"
        viewBox="0 0 24 24"
        fill="none"
        stroke="currentColor"
        stroke-width="2"
      >
        <ellipse cx="12" cy="5" rx="9" ry="3" />
        <path d="M3 5v6c0 1.7 4 3 9 3s9-1.3 9-3V5" />
        <path d="M3 11v6c0 1.7 4 3 9 3s9-1.3 9-3v-6" />
      </svg>
      <svg v-else viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2">
        <path d="M22 12h-6l-2 3h-4l-2-3H2" />
        <path
          d="M5.45 5.11L2 12v6a2 2 0 002 2h16a2 2 0 002-2v-6l-3.45-6.89A2 2 0 0016.76 4H7.24a2 2 0 00-1.79 1.11z"
        />
      </svg>
    </div>
    <div v-if="title && !compact" class="state-title">{{ title }}</div>
    <div :class="compact ? '' : 'state-message'">
      <slot>{{ message }}</slot>
    </div>
    <div v-if="$slots.actions && !compact" class="state-actions">
      <slot name="actions" />
    </div>
  </div>
</template>
