<script setup lang="ts">
// "back in 3d 4h": the time left on a hold. Like every date in the UI, a click toggles the full
// local date-time.
import { computed, ref } from 'vue'
import { useNow } from '@/composables/useNow'
import { fullDateTime, holdRemaining } from '@/format'

const props = defineProps<{ iso: string }>()
const now = useNow()
const expanded = ref(false)
const clock = () => Math.max(now.value, Date.now())
const rel = computed(() => holdRemaining(props.iso, clock()))
const full = computed(() => fullDateTime(props.iso))
</script>

<template>
  <button
    type="button"
    class="lg-time inline cursor-pointer rounded-sm border-0 bg-transparent p-0 text-inherit [font-weight:inherit] hover:underline"
    v-tooltip="full"
    :aria-label="expanded ? `Back on ${full}. Show time left` : `Back in ${rel}, on ${full}. Show full date`"
    :aria-pressed="expanded"
    @click.stop.prevent="expanded = !expanded"
  >
    <time :datetime="iso">{{ expanded ? full : rel }}</time>
  </button>
</template>
