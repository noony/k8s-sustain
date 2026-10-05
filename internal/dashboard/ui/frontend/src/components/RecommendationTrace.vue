<script setup lang="ts">
import { computed } from 'vue'
import type { ContainerTrace, TraceStage } from '../lib/api'
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

// A stage missing here is shown under its own name: a new stage needs no
// change to render.
const stageNames: Record<string, string> = {
  percentile: 'Usage percentile',
  oomFloor: 'OOM floor',
  headroom: 'With headroom',
  clamp: 'Min/max clamp',
  coordination: 'Autoscaler coordination',
  keepLimit: 'Limit kept',
  noLimit: 'Limit removed',
  equalsToRequest: 'Limit equal to request',
  requestsLimitsRatio: 'Limit at a ratio of the request',
  keepLimitRequestRatio: 'Limit at the kept ratio',
  guard: 'Limit raised to the request',
}

function cell(st: TraceStage | undefined, fmt: Format): string | undefined {
  if (!st) return undefined
  const value = st.removed ? 'removed' : st.value ? fmt(st.value) : 'none'
  const facts = Object.entries(st.facts ?? {})
    .sort(([a], [b]) => a.localeCompare(b))
    .map(([k, v]) => `${k}: ${v}`)
  return [value, ...facts].join(' · ')
}

// The stage names of every list, each list's in its own order; a name only
// one list has goes right after the name it follows there, or last.
function mergeOrder(...lists: string[][]): string[] {
  const out: string[] = []
  for (const list of lists) {
    let at = out.length
    for (const name of list) {
      const i = out.indexOf(name)
      if (i >= 0) {
        at = i + 1
        continue
      }
      out.splice(at, 0, name)
      at++
    }
  }
  return out
}

function side(key: 'request' | 'limit') {
  const c = props.trace.cpu?.[key] ?? []
  const m = props.trace.memory?.[key] ?? []
  return mergeOrder(
    c.map((s) => s.stage),
    m.map((s) => s.stage),
  ).map((name) => ({
    key: `${key}:${name}`,
    stage: stageNames[name] ?? name,
    cpu: cell(
      c.find((s) => s.stage === name),
      cpu,
    ),
    memory: cell(
      m.find((s) => s.stage === name),
      mem,
    ),
  }))
}

const rows = computed(() => [...side('request'), ...side('limit')])
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
        <tr v-for="r in rows" :key="r.key" :data-stage="r.key">
          <td>{{ r.stage }}</td>
          <td>{{ r.cpu ?? '–' }}</td>
          <td>{{ r.memory ?? '–' }}</td>
        </tr>
      </tbody>
    </table>
  </details>
</template>
