<script setup lang="ts">
import { computed } from 'vue'
import type { ContainerTrace, ResourceTrace } from '../lib/api'
import { parseCPUQuantity, parseMemoryQuantity } from '../lib/format'

const props = defineProps<{ trace: ContainerTrace }>()

type Format = (q: string) => string

function round(v: number, digits: number): number {
  return Number(v.toFixed(digits))
}

// One unit per resource so the stages read as a progression; the stored
// percentile is nanocore- or byte-precise.
const cpu: Format = (q) => `${round(parseCPUQuantity(q) * 1000, 2)}m`
const mem: Format = (q) => `${round(parseMemoryQuantity(q) / 2 ** 20, 1)}Mi`

function value(t: ResourceTrace | undefined, q: string | undefined, fmt: Format) {
  return t && q ? fmt(q) : undefined
}

function oomFloor(t: ResourceTrace | undefined, fmt: Format) {
  const f = t?.oomFloor
  if (!f) return undefined
  return fmt(f.value) + (f.determined ? ' · set the request' : '')
}

function coordination(t: ResourceTrace | undefined, fmt: Format) {
  const c = t?.coordination
  if (!c) return undefined
  let s = `×${round(c.overheadFactor, 2)} overhead`
  if (c.replicaFactor !== undefined) s += `, ×${round(c.replicaFactor, 2)} replica`
  s += ` → ${fmt(c.scaled)}`
  if (fmt(c.value) !== fmt(c.scaled)) s += `, clamped to ${fmt(c.value)}`
  return s
}

function limit(t: ResourceTrace | undefined, fmt: Format) {
  if (!t) return undefined
  if (t.removeLimit) return 'removed'
  return t.limit ? fmt(t.limit) : 'kept'
}

const rows = computed(() => {
  const { cpu: c, memory: m } = props.trace
  return [
    {
      stage: 'Usage percentile',
      cpu: value(c, c?.percentile, cpu),
      memory: value(m, m?.percentile, mem),
    },
    { stage: 'OOM floor', cpu: oomFloor(c, cpu), memory: oomFloor(m, mem) },
    {
      stage: 'With headroom',
      cpu: value(c, c?.withHeadroom, cpu),
      memory: value(m, m?.withHeadroom, mem),
    },
    { stage: 'Min/max clamp', cpu: value(c, c?.clamped, cpu), memory: value(m, m?.clamped, mem) },
    { stage: 'Autoscaler coordination', cpu: coordination(c, cpu), memory: coordination(m, mem) },
    { stage: 'Limit', cpu: limit(c, cpu), memory: limit(m, mem) },
  ].filter((r) => r.cpu !== undefined || r.memory !== undefined)
})
</script>

<template>
  <details v-if="rows.length > 0" class="trace">
    <summary>How this was computed</summary>
    <table class="trace-table" data-test="trace">
      <thead>
        <tr>
          <th>Stage</th>
          <th>CPU</th>
          <th>Memory</th>
        </tr>
      </thead>
      <tbody>
        <tr v-for="r in rows" :key="r.stage" :data-stage="r.stage">
          <td>{{ r.stage }}</td>
          <td>{{ r.cpu ?? '–' }}</td>
          <td>{{ r.memory ?? '–' }}</td>
        </tr>
      </tbody>
    </table>
  </details>
</template>
