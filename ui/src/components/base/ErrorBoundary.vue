<script setup lang="ts">
// Catches render/setup/lifecycle errors from the wrapped view so one broken page does not
// blank the whole application. Errors from event handlers and watcher callbacks are left to
// the app error handler (logged): the page is still usable and replacing it would lose work.
// "Try again" re-mounts the wrapped content; any route change resets the boundary.
import { IconAlertTriangle, IconRefresh } from '@tabler/icons-vue'
import { computed, nextTick, onErrorCaptured, ref, shallowRef, watch } from 'vue'
import { useRoute, useRouter, type RouteLocationRaw } from 'vue-router'
import { isChunkLoadError, markStaleBuild } from '@/staleBuild'
import BaseButton from './BaseButton.vue'

const props = withDefaults(defineProps<{ homeTo?: RouteLocationRaw; homeLabel?: string; fullscreen?: boolean }>(), {
  homeTo: () => ({ name: 'home' }),
  homeLabel: 'Go to queue',
  fullscreen: false,
})

const route = useRoute()
const router = useRouter()
const failure = shallowRef<Error | null>(null)
const generation = ref(0)
const heading = ref<HTMLElement | null>(null)
const showStack = import.meta.env.DEV

// Vue reports where an error came from as text in development and as an error-reference URL
// ending in the code in production; both are matched.
const NON_FATAL = new Set(['native event handler', 'component event handler', 'watcher callback', 'watcher cleanup function', '3', '4', '5', '6'])
function isFatal(info: string): boolean {
  const code = info.match(/#runtime-(\w+)$/)?.[1] ?? info
  return !NON_FATAL.has(code)
}

onErrorCaptured((err, _instance, info) => {
  if (isChunkLoadError(err)) {
    markStaleBuild()
    return false
  }
  if (!isFatal(info)) return true
  failure.value = err instanceof Error ? err : new Error(String(err))
  console.error('[ui] view failed to render:', err, info)
  void nextTick(() => heading.value?.focus())
  return false
})

const summary = computed(() => {
  const e = failure.value
  if (!e) return ''
  return e.message ? (e.name && e.name !== 'Error' ? `${e.name}: ${e.message}` : e.message) : e.name || 'Unknown error'
})

function reset() {
  failure.value = null
  generation.value++
}
async function goHome() {
  await router.push(props.homeTo).catch(() => undefined)
  reset()
}

watch(() => route.fullPath, () => {
  if (failure.value) reset()
})
</script>

<template>
  <div v-if="failure" class="flex flex-1 items-start justify-center overflow-auto px-5 py-10" :class="fullscreen ? 'min-h-screen bg-bg text-text' : ''">
    <section class="lg-panel flex w-full max-w-lg flex-col gap-3 p-5" aria-labelledby="error-boundary-title">
      <div class="flex items-start gap-2.5">
        <IconAlertTriangle :size="18" stroke="1.75" aria-hidden="true" class="mt-0.5 flex-none text-danger" />
        <div class="flex min-w-0 flex-col gap-1">
          <!-- h1: it replaces the page's view (and its heading). -->
          <h1 id="error-boundary-title" ref="heading" tabindex="-1" class="m-0 text-[15px] font-semibold outline-none">This page stopped working</h1>
          <p class="m-0 text-muted">Something went wrong while showing this page. Your other tabs and saved work are not affected.</p>
        </div>
      </div>
      <p class="lg-mono m-0 rounded border border-border bg-subtle px-3 py-2 text-xs break-words">{{ summary }}</p>
      <details v-if="showStack && failure.stack" class="text-xs">
        <summary class="cursor-pointer text-muted">Stack trace (development only)</summary>
        <pre class="lg-mono mt-2 max-h-64 overflow-auto whitespace-pre-wrap text-[11px]">{{ failure.stack }}</pre>
      </details>
      <div class="flex gap-2">
        <BaseButton variant="primary" @click="reset">
          <template #icon><IconRefresh :size="14" stroke="1.75" aria-hidden="true" /></template>
          Try again
        </BaseButton>
        <BaseButton @click="goHome">{{ homeLabel }}</BaseButton>
      </div>
    </section>
  </div>
  <div v-else :key="generation" class="contents"><slot /></div>
</template>
