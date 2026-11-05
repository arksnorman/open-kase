// Shared by InlineEditField: the one field currently in edit mode (opening another cancels it).
import { shallowRef } from 'vue'

export const activeInlineEdit = shallowRef<symbol | null>(null)
