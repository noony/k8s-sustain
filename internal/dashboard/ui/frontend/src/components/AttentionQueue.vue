<script setup lang="ts">
import { ref } from 'vue'
import type { AttentionGroup, AttentionState } from '../lib/api'

defineProps<{ groups: Record<AttentionState, AttentionGroup> }>()
const open = ref<Record<AttentionState, boolean>>({
  conflicted: true,
  blocked: true,
  'at-risk': true,
  drifted: true,
})

// Risk-state precedence, so the most actionable group comes first.
const groupMeta: { state: AttentionState; label: string; cls: string }[] = [
  { state: 'conflicted', label: 'Conflicted', cls: 'risk-conflicted' },
  { state: 'blocked', label: 'Blocked', cls: 'risk-blocked' },
  { state: 'at-risk', label: 'Risk', cls: 'risk-risk' },
  { state: 'drifted', label: 'Drift', cls: 'risk-drift' },
]
</script>

<template>
  <div class="aq">
    <div v-for="g in groupMeta" :key="g.state" class="aq-group">
      <button class="aq-header" :class="g.cls" @click="open[g.state] = !open[g.state]">
        <span class="aq-caret">{{ open[g.state] ? '▾' : '▸' }}</span>
        <span class="aq-title">{{ g.label }}</span>
        <span class="aq-count">{{ groups[g.state].total }}</span>
      </button>
      <div v-if="open[g.state] && groups[g.state].rows.length" class="aq-body">
        <RouterLink
          v-for="r in groups[g.state].rows"
          :key="r.namespace + '/' + r.kind + '/' + r.name"
          class="aq-row"
          :to="`/workloads/${r.namespace}/${r.kind}/${r.name}`"
        >
          <span class="aq-ns">{{ r.namespace }}</span>
          <span class="aq-kind">{{ r.kind }}</span>
          <span class="aq-name">{{ r.name }}</span>
          <span class="aq-signal"
            >{{ r.signal }}<span v-if="r.detail"> · {{ r.detail }}</span></span
          >
        </RouterLink>
        <RouterLink
          v-if="groups[g.state].total > groups[g.state].rows.length"
          class="aq-more"
          :to="`/workloads?risk=${g.state}`"
          >View all {{ groups[g.state].total }}</RouterLink
        >
      </div>
      <div v-else-if="open[g.state]" class="aq-empty">No items.</div>
    </div>
  </div>
</template>
