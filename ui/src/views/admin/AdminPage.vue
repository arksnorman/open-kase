<script setup lang="ts">
withDefaults(defineProps<{ title: string; description?: string; section?: string }>(), { section: 'Settings' })
</script>

<template>
  <!-- Below 640 px the shell's top bar names the section and the heading the page: the bar keeps
       only the toolbar (page actions), or disappears when there is none. -->
  <div
    class="flex h-[52px] flex-none items-center gap-4 border-b border-border bg-surface px-5 max-sm:justify-end"
    :class="$slots.toolbar ? '' : 'max-sm:hidden'"
  >
    <nav aria-label="Breadcrumb" class="hidden min-w-0 items-center gap-1.5 text-muted sm:flex">
      <span class="lg-ell">{{ section }}</span>
      <span aria-hidden="true">/</span>
      <span aria-current="page" class="lg-ell font-semibold text-text">{{ title }}</span>
    </nav>
    <div class="hidden flex-1 sm:block"></div>
    <slot name="toolbar" />
  </div>
  <div class="flex flex-col gap-4 px-5 py-5">
    <!-- Page actions (e.g. "New group") sit on the heading row, right of the title and description. -->
    <div class="flex flex-wrap items-start gap-x-6 gap-y-3">
      <div class="flex min-w-[min(100%,20rem)] flex-1 items-start gap-3">
        <!-- Optional leading avatar (user pages). -->
        <slot name="avatar" />
        <div class="flex min-w-0 flex-col gap-0.5">
          <h1 class="m-0 text-lg font-semibold tracking-[-0.01em]">{{ title }}</h1>
          <p v-if="description" class="m-0 max-w-[80ch] text-muted">{{ description }}</p>
        </div>
      </div>
      <div v-if="$slots.actions" class="flex flex-none flex-wrap items-center gap-2"><slot name="actions" /></div>
    </div>
    <slot />
  </div>
</template>
