<script setup lang="ts">
import { timeRangeOptions } from '../lib/api'

const props = defineProps<{
  modelValue: string
}>()

const emit = defineEmits<{
  'update:modelValue': [value: string]
}>()
</script>

<template>
  <div class="time-range-group">
    <button
      v-for="opt in timeRangeOptions"
      :key="opt.window"
      :class="{ active: opt.window === props.modelValue }"
      type="button"
      @click="emit('update:modelValue', opt.window)"
    >
      {{ opt.label }}
    </button>
  </div>
</template>
