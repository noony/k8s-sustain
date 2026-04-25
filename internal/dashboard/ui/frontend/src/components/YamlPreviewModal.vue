<script setup lang="ts">
defineProps<{ open: boolean; title: string; yaml: string }>()
const emit = defineEmits<{ close: [] }>()
</script>

<template>
  <div v-if="open" class="modal-backdrop" @click="emit('close')">
    <div class="modal-body" @click.stop>
      <div class="modal-head">
        <h3>{{ title }}</h3>
        <button class="modal-close" @click="emit('close')">×</button>
      </div>
      <pre class="modal-yaml"><code>{{ yaml }}</code></pre>
    </div>
  </div>
</template>
