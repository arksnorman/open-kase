<script setup lang="ts">
// Status pill: colour and dot shape per lifecycle category. A long custom name is cut with an
// ellipsis inside the space it gets; the full name is in the tooltip.
import type { TicketStatus } from '@/api/types'
import { statusDotClass, statusToneClass } from '@/format'

defineProps<{ status: TicketStatus; /** Tooltip; defaults to the status name. */ title?: string }>()
</script>

<template>
  <span class="lg-pill lg-status" :class="statusToneClass(status.category)" v-tooltip="title ?? status.name">
    <span aria-hidden="true" class="lg-dot" :class="statusDotClass(status.category)"></span><span class="lg-ell">{{ status.name }}</span>
  </span>
</template>
