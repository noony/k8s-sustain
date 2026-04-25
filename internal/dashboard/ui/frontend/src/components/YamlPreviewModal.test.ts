import { mount } from '@vue/test-utils'
import YamlPreviewModal from './YamlPreviewModal.vue'
import { describe, it, expect } from 'vitest'

describe('YamlPreviewModal', () => {
  it('shows yaml when open', () => {
    const w = mount(YamlPreviewModal, {
      props: { open: true, title: 'Policy', yaml: 'apiVersion: v1' },
    })
    expect(w.text()).toContain('apiVersion: v1')
  })
  it('does not render when closed', () => {
    const w = mount(YamlPreviewModal, { props: { open: false, title: 'Policy', yaml: '' } })
    expect(w.find('.modal-body').exists()).toBe(false)
  })
})
