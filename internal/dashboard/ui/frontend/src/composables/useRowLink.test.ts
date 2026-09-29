import { mount } from '@vue/test-utils'
import { describe, it, expect, vi, afterEach } from 'vitest'
import { defineComponent, h } from 'vue'
import { createRouter, createMemoryHistory } from 'vue-router'
import { useRowLink } from './useRowLink'

const Row = defineComponent({
  setup() {
    const { openRow } = useRowLink()
    return () =>
      h('div', {
        class: 'row',
        onClick: (e: MouseEvent) => openRow('/target', e),
        onAuxclick: (e: MouseEvent) => openRow('/target', e),
      })
  },
})

async function mountRow() {
  const router = createRouter({
    history: createMemoryHistory(),
    routes: [
      { path: '/', component: { template: '<div />' } },
      { path: '/target', component: { template: '<div />' } },
    ],
  })
  await router.push('/')
  const w = mount(Row, { global: { plugins: [router] } })
  return { w, router }
}

describe('useRowLink', () => {
  afterEach(() => vi.restoreAllMocks())

  it('navigates in place on a plain click', async () => {
    const open = vi.spyOn(window, 'open').mockReturnValue(null)
    const { w, router } = await mountRow()
    await w.find('.row').trigger('click')
    await router.isReady()
    await new Promise((r) => setTimeout(r, 0))
    expect(router.currentRoute.value.path).toBe('/target')
    expect(open).not.toHaveBeenCalled()
  })

  it.each([
    ['Cmd', { metaKey: true }],
    ['Ctrl', { ctrlKey: true }],
    ['Shift', { shiftKey: true }],
  ])('opens a new tab on %s-click', async (_, mods) => {
    const open = vi.spyOn(window, 'open').mockReturnValue(null)
    const { w, router } = await mountRow()
    await w.find('.row').trigger('click', mods)
    expect(open).toHaveBeenCalledWith('/target', '_blank')
    expect(router.currentRoute.value.path).toBe('/')
  })

  it('opens a new tab on middle-click and ignores right-click', async () => {
    const open = vi.spyOn(window, 'open').mockReturnValue(null)
    const { w, router } = await mountRow()
    await w.find('.row').trigger('auxclick', { button: 2 })
    expect(open).not.toHaveBeenCalled()
    await w.find('.row').trigger('auxclick', { button: 1 })
    expect(open).toHaveBeenCalledWith('/target', '_blank')
    expect(router.currentRoute.value.path).toBe('/')
  })
})
