<script setup lang="ts">
import { computed } from 'vue'
import { renderMarkdown } from '@/markdown'

const props = defineProps<{ source: string }>()
// renderMarkdown disables raw HTML and sanitizes the output, so v-html is safe here.
const html = computed(() => renderMarkdown(props.source))
</script>

<template>
  <div class="lg-prose" v-html="html"></div>
</template>
