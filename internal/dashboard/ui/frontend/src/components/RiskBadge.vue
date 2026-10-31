<script setup lang="ts">
import { computed } from 'vue'
import type { RiskState } from '../lib/api'
import { riskStateMetaOf } from '../lib/risk'

const props = defineProps<{ state?: RiskState | string }>()
const meta = computed(() => riskStateMetaOf(props.state))
</script>

<template>
  <span class="risk-badge" :class="meta?.cls ?? 'risk-unknown'">{{ meta?.label ?? '-' }}</span>
</template>
