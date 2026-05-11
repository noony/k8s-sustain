<script setup lang="ts">
defineProps<{
  title: string
  subtitle?: string
}>()
</script>

<template>
  <div class="page-header row-between">
    <div>
      <h1>
        <slot name="title">{{ title }}</slot>
      </h1>
      <p v-if="subtitle || $slots.subtitle">
        <slot name="subtitle">{{ subtitle }}</slot>
      </p>
      <div v-if="$slots.meta" class="row mt-2">
        <slot name="meta" />
      </div>
    </div>
    <div v-if="$slots.actions" class="row">
      <slot name="actions" />
    </div>
  </div>
</template>
