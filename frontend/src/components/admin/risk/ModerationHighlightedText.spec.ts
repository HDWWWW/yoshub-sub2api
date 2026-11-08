import { describe, expect, it } from 'vitest'
import { mount } from '@vue/test-utils'
import ModerationHighlightedText from './ModerationHighlightedText.vue'

describe('moderation keyword highlighting', () => {
  it('matches literal regex characters and mixed case without interpreting HTML', () => {
    const text = '<script>alert(1)</script> A+B[0] 🎬 a+b[0]'
    const wrapper = mount(ModerationHighlightedText, { props: { text, keyword: 'a+b[0]' } })
    expect(wrapper.text()).toBe(text)
    expect(wrapper.find('script').exists()).toBe(false)
    expect(wrapper.findAll('mark').map(mark => mark.text())).toEqual(['A+B[0]', 'a+b[0]'])
  })
})
