<script setup lang="ts">
// Monospace block for secrets, tokens and snippets. `selectAll` selects the whole value on click
// so it can be copied in one go; `pre` keeps whitespace (code samples) instead of wrapping, and
// a sample wider than its box can be focused to scroll it with the keyboard.
import { ref } from 'vue'
import { useOverflowX } from './overflow'

withDefaults(defineProps<{ pre?: boolean; selectAll?: boolean; inline?: boolean }>(), { pre: false, selectAll: false, inline: false })
const preEl = ref<HTMLElement | null>(null)
const overflowing = useOverflowX(preEl)
</script>

<template>
  <pre
    v-if="pre"
    ref="preEl"
    class="lg-mono m-0 overflow-x-auto rounded-md border border-border bg-subtle p-3 text-xs"
    :tabindex="overflowing ? 0 : undefined"
  ><code><slot /></code></pre>
  <code
    v-else
    class="lg-mono rounded-md border border-border bg-subtle break-all"
    :class="[inline ? 'px-2.5 py-1.5 text-[13px] tracking-wide' : 'block p-3 text-xs', selectAll ? 'select-all' : '']"
  ><slot /></code>
</template>
