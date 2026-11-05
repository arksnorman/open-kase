<script setup lang="ts">
import { BaseButton, BaseCard, EmptyState } from '@/components/base'

defineProps<{ message?: string }>()
</script>

<template>
  <main class="flex min-h-screen items-center justify-center bg-bg px-4">
    <BaseCard as="div" :padded="false" class="w-full max-w-md">
      <EmptyState title="Not found" title-tag="h1" :description="message ?? 'This page does not exist, or you do not have access to it.'">
        <template #actions><BaseButton to="/">Go to your work</BaseButton></template>
      </EmptyState>
    </BaseCard>
  </main>
</template>
