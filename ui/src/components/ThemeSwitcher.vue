<script setup lang="ts">
import { IconDeviceDesktop, IconMoon, IconSun } from '@tabler/icons-vue'
import { computed } from 'vue'
import type { ThemeChoice } from '@/api/types'
import { SegmentedControl, type Segment } from '@/components/base'
import { usePreferencesStore } from '@/stores/preferences'

defineProps<{ compact?: boolean }>()
const prefs = usePreferencesStore()
const options: Segment<ThemeChoice>[] = [
  { value: 'system', label: 'System', icon: IconDeviceDesktop },
  { value: 'light', label: 'Light', icon: IconSun },
  { value: 'dark', label: 'Dark', icon: IconMoon },
]
const theme = computed({
  get: () => prefs.theme,
  set: (v: ThemeChoice) => void prefs.setTheme(v),
})
</script>

<template>
  <SegmentedControl v-model="theme" :options="options" label="Theme" variant="quiet" :size="compact ? 'sm' : 'md'" :block="compact" />
</template>
