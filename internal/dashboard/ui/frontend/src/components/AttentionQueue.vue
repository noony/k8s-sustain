<script setup lang="ts">
import { ref } from 'vue'
import type { AttentionRow } from '../lib/api'

defineProps<{ groups: { risk: AttentionRow[]; drift: AttentionRow[]; blocked: AttentionRow[] } }>()
const open = ref({ risk: true, drift: true, blocked: true })
const emit = defineEmits<{ select: [row: AttentionRow] }>()

const groupMeta: Record<string, { label: string; cls: string }> = {
  risk: { label: 'Risk', cls: 'risk-risk' },
  drift: { label: 'Drift', cls: 'risk-drift' },
  blocked: { label: 'Blocked', cls: 'risk-blocked' },
}
</script>

<template>
  <div class="aq">
    <div v-for="(rows, key) in groups" :key="key" class="aq-group">
      <button class="aq-header" :class="groupMeta[key].cls" @click="open[key] = !open[key]">
        <span class="aq-caret">{{ open[key] ? '▾' : '▸' }}</span>
        <span class="aq-title">{{ groupMeta[key].label }}</span>
        <span class="aq-count">{{ rows.length }}</span>
      </button>
      <div v-if="open[key] && rows.length" class="aq-body">
        <button
          v-for="r in rows"
          :key="r.namespace + '/' + r.kind + '/' + r.name"
          class="aq-row"
          @click="emit('select', r)"
        >
          <span class="aq-ns">{{ r.namespace }}</span>
          <span class="aq-kind">{{ r.kind }}</span>
          <span class="aq-name">{{ r.name }}</span>
          <span class="aq-signal"
            >{{ r.signal }}<span v-if="r.detail"> · {{ r.detail }}</span></span
          >
        </button>
      </div>
      <div v-else-if="open[key]" class="aq-empty">No items.</div>
    </div>
  </div>
</template>
