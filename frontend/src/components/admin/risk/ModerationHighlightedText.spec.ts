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

  it('highlights all groups, preferring complete overlapping words and escaping markup', () => {
    const text = '<img src=x onerror=alert(1)> 复合物 A+B[0] 猫复合物'
    const wrapper = mount(ModerationHighlightedText, { props: {
      text, keyword: '猫', keywords: ['', '物', '复合物', 'a+b[0]', '复合物', '<img'],
    } })
    expect(wrapper.text()).toBe(text)
    expect(wrapper.find('img').exists()).toBe(false)
    expect(wrapper.findAll('mark').map(mark => mark.text())).toEqual(['<img', '复合物', 'A+B[0]', '猫', '复合物'])
  })
})
