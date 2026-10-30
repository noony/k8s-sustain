<script setup lang="ts">
import { ref } from 'vue'
import type { AttentionGroup, AttentionState } from '../lib/api'
import { attentionStateMeta } from '../lib/risk'

defineProps<{ groups: Record<AttentionState, AttentionGroup> }>()
const collapsed = ref<Partial<Record<AttentionState, boolean>>>({})
</script>

<template>
  <div class="aq">
    <div v-for="g in attentionStateMeta" :key="g.state" class="aq-group">
      <button class="aq-header" :class="g.cls" @click="collapsed[g.state] = !collapsed[g.state]">
        <span class="aq-caret">{{ collapsed[g.state] ? '▸' : '▾' }}</span>
        <span class="aq-title">{{ g.label }}</span>
        <span class="aq-count">{{ groups[g.state].total }}</span>
      </button>
      <div v-if="!collapsed[g.state] && groups[g.state].rows.length" class="aq-body">
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
          :to="g.to"
          >View all {{ groups[g.state].total }}</RouterLink
        >
      </div>
      <div v-else-if="!collapsed[g.state]" class="aq-empty">No items.</div>
    </div>
  </div>
</template>
