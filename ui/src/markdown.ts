// Safe Markdown → HTML for stored message bodies: markdown-it with raw HTML disabled, then
// DOMPurify as defence in depth. Links open in a new tab without referrer/opener.

import DOMPurify from 'dompurify'
import MarkdownIt from 'markdown-it'

const md = new MarkdownIt({ html: false, linkify: true, breaks: true, typographer: false })

// Allow only web and mail links; markdown-it already rejects javascript:/vbscript:/data: (except images).
const SAFE_LINK = /^(https?:|mailto:)/i
const defaultValidate = md.validateLink.bind(md)
md.validateLink = (url: string) => defaultValidate(url) && (SAFE_LINK.test(url.trim()) || /^[^:]*$/.test(url.trim()))

DOMPurify.addHook('afterSanitizeAttributes', (node) => {
  if (node.tagName === 'A') {
    node.setAttribute('target', '_blank')
    node.setAttribute('rel', 'noopener noreferrer nofollow')
  }
  if (node.tagName === 'IMG') {
    node.setAttribute('loading', 'lazy')
    node.setAttribute('referrerpolicy', 'no-referrer')
  }
})

export function renderMarkdown(source: string): string {
  return DOMPurify.sanitize(md.render(source), {
    USE_PROFILES: { html: true },
    ADD_ATTR: ['target'],
    FORBID_TAGS: ['style', 'form', 'input', 'button', 'textarea', 'select', 'iframe', 'object', 'embed'],
    FORBID_ATTR: ['style'],
  })
}
