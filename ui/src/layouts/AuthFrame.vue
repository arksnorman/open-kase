<script setup lang="ts">
// Public page frame (Ledger sign-in): brand bar, one centred 388px column with a heading, the
// form card and the theme switcher, and the version footer. Used by sign-in, password reset and
// invitation pages.
import BrandMark from '@/components/BrandMark.vue'
import ThemeSwitcher from '@/components/ThemeSwitcher.vue'
import { product } from '@/product'

defineProps<{ title: string; description?: string }>()
</script>

<template>
  <div class="flex min-h-screen flex-col bg-bg text-[14px] leading-[1.45] text-text">
    <header class="flex h-14 items-center justify-between border-b border-line bg-surface px-6">
      <BrandMark size="md" />
    </header>

    <main class="flex flex-1 items-center justify-center px-4 py-12">
      <div class="flex w-full max-w-[388px] flex-col gap-5">
        <div class="flex flex-col gap-1">
          <h1 class="m-0 text-[22px] font-semibold tracking-[-0.015em]">{{ title }}</h1>
          <p v-if="description" class="m-0 text-[13.5px] text-muted">{{ description }}</p>
        </div>

        <slot name="notice" />
        <slot />

        <div class="flex items-center justify-between gap-3">
          <span aria-hidden="true" class="text-[13px] text-muted">Theme</span>
          <ThemeSwitcher />
        </div>
      </div>
    </main>

    <footer class="flex justify-center border-t border-line px-6 py-4">
      <span class="lg-mono text-xs text-muted">Self-hosted · v{{ product.version }}</span>
    </footer>
  </div>
</template>
