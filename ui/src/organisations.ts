// Organisation pickers: server-searched options (the list can grow without bound), shared by the new
// ticket form, the ticket details, the queue filter and the Settings page.
import { organisationsApi } from '@/api/endpoints'
import type { OrganisationRef } from '@/api/types'
import type { SelectOption } from '@/components/base'

/** Value of the "No organisation" entry of a picker that can clear the organisation. */
export const NO_ORGANISATION = '__none'

export function organisationOption(o: OrganisationRef | { id: string; name: string; externalRef: string | null }): SelectOption<string> {
  return { value: o.id, label: o.name, hint: o.externalRef ?? undefined, keywords: o.externalRef ?? undefined }
}

/** Loader for a searchable select: the first page of organisations matching the search text. */
export async function loadOrganisations(q: string, signal: AbortSignal, withNone = false, active: 'true' | 'all' = 'true'): Promise<SelectOption<string>[]> {
  const page = await organisationsApi.list(null, q, { signal }, active)
  const out = page.items.map(organisationOption)
  if (page.nextCursor) out.push({ value: '__more', label: 'More organisations match; type to narrow the list', disabled: true })
  return withNone && !q.trim() ? [{ value: NO_ORGANISATION, label: 'No organisation' }, ...out] : out
}
