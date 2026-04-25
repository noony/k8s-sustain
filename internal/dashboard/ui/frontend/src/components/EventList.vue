<script setup lang="ts">
import { timeAgo } from '../lib/format'
import type { ActivityItem } from '../lib/api'
defineProps<{ items: ActivityItem[] }>()
</script>

<template>
  <div class="event-list">
    <div v-if="!items.length" class="empty-state"><p>No recent activity.</p></div>
    <div v-for="(e, i) in items" :key="i" class="event-row">
      <span class="event-time">{{ timeAgo(e.timestamp) }}</span>
      <span class="event-target">{{ e.namespace }}/{{ e.kind }}/{{ e.name }}</span>
      <span class="event-reason">{{ e.reason }}</span>
      <span class="event-msg">{{ e.message }}</span>
    </div>
  </div>
</template>
