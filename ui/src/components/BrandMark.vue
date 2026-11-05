<script setup lang="ts">
import { IconTicket } from '@tabler/icons-vue'
import { useBrandStore } from '@/stores/brand'

withDefaults(defineProps<{ size?: 'sm' | 'md' }>(), { size: 'sm' })
const brand = useBrandStore()
</script>

<template>
  <span class="flex items-center gap-2.5">
    <span
      aria-hidden="true"
      class="inline-flex items-center justify-center rounded-md bg-primary text-primary-fg"
      :class="size === 'md' ? 'size-[26px]' : 'size-6'"
    >
      <IconTicket :size="size === 'md' ? 16 : 15" stroke="1.75" />
    </span>
    <span class="font-semibold tracking-[-0.01em]" :class="[size === 'md' ? 'text-[15px]' : 'text-[14px]', { invisible: !brand.ready }]">
      {{ brand.name }}
    </span>
  </span>
</template>
