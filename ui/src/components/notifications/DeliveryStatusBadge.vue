<script setup lang="ts">
import { computed } from 'vue'
import type { DeliveryStatus } from '@/api/types'
import { BaseBadge } from '@/components/base'
import { deliveryStatusExplanation, deliveryStatusLabel, deliveryStatusTone } from '@/deliveries'

const props = defineProps<{ status: DeliveryStatus }>()
const explanation = computed(() => deliveryStatusExplanation(props.status))
</script>

<template>
  <BaseBadge :tone="deliveryStatusTone(status)" v-tooltip="explanation">
    {{ deliveryStatusLabel(status) }}<span class="sr-only">: {{ explanation }}</span>
  </BaseBadge>
</template>
