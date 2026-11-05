import { computed, inject, useId, type ComputedRef, type InjectionKey } from 'vue'

export interface FieldContext {
  id: string
  /** id of the visible <label>, for controls that are not labelable elements (aria-labelledby). */
  labelId: string
  describedBy: ComputedRef<string | undefined>
  invalid: ComputedRef<boolean>
  required: ComputedRef<boolean>
}

export const FieldKey: InjectionKey<FieldContext> = Symbol('form-field')

/** Wires a control to its surrounding FormField (id, aria-describedby, aria-invalid). */
export function useFieldControl(props: { id?: string | undefined; invalid?: boolean | undefined }) {
  const field = inject(FieldKey, null)
  const fallbackId = useId()
  return {
    id: computed(() => props.id ?? field?.id ?? fallbackId),
    labelId: computed(() => field?.labelId),
    describedBy: computed(() => field?.describedBy.value),
    invalid: computed(() => (props.invalid || field?.invalid.value) ?? false),
    required: computed(() => field?.required.value ?? false),
  }
}
