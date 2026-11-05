<script setup lang="ts">
import { IconEye, IconEyeOff } from '@tabler/icons-vue'
import { ref } from 'vue'
import { useFieldControl } from './field'
import IconButton from './IconButton.vue'

defineOptions({ inheritAttrs: false })
const props = withDefaults(defineProps<{ id?: string; invalid?: boolean; size?: 'md' | 'lg' }>(), { size: 'md' })
const model = defineModel<string>({ default: '' })
const { id, describedBy, invalid, required } = useFieldControl(props)
const visible = ref(false)
</script>

<template>
  <div class="relative">
    <input
      :id="id"
      v-model="model"
      :type="visible ? 'text' : 'password'"
      class="lg-input pr-10"
      :class="size === 'lg' ? 'h-10 text-[14px]' : ''"
      :aria-invalid="invalid ? 'true' : undefined"
      :aria-describedby="describedBy"
      :required="required || undefined"
      v-bind="$attrs"
    />
    <IconButton
      class="absolute top-1/2 right-1 -translate-y-1/2"
      :label="visible ? 'Hide password' : 'Show password'"
      :pressed="visible"
      @click="visible = !visible"
    >
      <IconEyeOff v-if="visible" :size="16" stroke="1.75" />
      <IconEye v-else :size="16" stroke="1.75" />
    </IconButton>
  </div>
</template>
