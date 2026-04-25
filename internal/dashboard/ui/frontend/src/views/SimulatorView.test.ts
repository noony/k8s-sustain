import { mount, flushPromises } from '@vue/test-utils'
import { describe, it, expect, vi } from 'vitest'
import SimulatorView from './SimulatorView.vue'
import * as api from '../lib/api'
vi.mock('../lib/api', async (o) => ({ ...((await o()) as object), api: vi.fn() }))

describe('SimulatorView', () => {
  it('renders form with Rollout option', async () => {
    ;(api.api as any).mockResolvedValue([])
    const w = mount(SimulatorView, { global: { stubs: ['TimeRangeSelector'] } })
    await flushPromises()
    expect(w.html()).toContain('<option value="Rollout">')
  })
})
