// Staged uploads for a composer or the new-ticket form. Each file is uploaded immediately
// (POST /attachments, field `file`) with progress; the returned ids are sent with
// the message or ticket. Staged files expire after 24 h on the server and are not part of drafts.

import { reactive, type Reactive } from 'vue'
import { ticketsApi } from '@/api/endpoints'
import { ApiError, errorMessage, isAbortError } from '@/api/errors'
import type { Attachment } from '@/api/types'

/** Contract limit: at most 5 attachments per message or ticket. */
export const MAX_ATTACHMENTS = 5

export interface StagedFile {
  localId: number
  filename: string
  size: number
  state: 'uploading' | 'ready' | 'error'
  progress: number
  error: string | null
  attachment: Attachment | null
  ctrl: AbortController | null
}

let seq = 0

function uploadErrorText(err: unknown): string {
  if (err instanceof ApiError) {
    if (err.is('attachmentTooLarge')) return 'Too large'
    if (err.status === 422) return err.fieldErrors[0]?.message ?? 'Not accepted'
    if (err.status === 0) return 'Network error'
    return err.title
  }
  return errorMessage(err)
}

export interface StagedAttachments {
  files: Reactive<StagedFile[]>
  add: (list: FileList | File[]) => string | null
  remove: (localId: number) => void
  clear: () => void
  uploading: () => boolean
  ids: () => string[]
}

export function createStagedAttachments(): StagedAttachments {
  const files = reactive<StagedFile[]>([])

  async function start(entry: StagedFile, file: File) {
    const ctrl = new AbortController()
    entry.ctrl = ctrl
    try {
      const a = await ticketsApi.uploadAttachment(file, {
        signal: ctrl.signal,
        onProgress: (f) => {
          entry.progress = f
        },
      })
      entry.attachment = a
      entry.size = a.size
      entry.filename = a.filename
      entry.state = 'ready'
      entry.progress = 1
    } catch (err) {
      if (isAbortError(err)) return
      entry.state = 'error'
      entry.error = uploadErrorText(err)
    } finally {
      entry.ctrl = null
    }
  }

  /** Starts uploads; returns a message when some files were not added (limit). */
  function add(list: FileList | File[]): string | null {
    const incoming = Array.from(list)
    const room = MAX_ATTACHMENTS - files.filter((f) => f.state !== 'error').length
    const accepted = incoming.slice(0, Math.max(0, room))
    for (const file of accepted) {
      files.push({
        localId: ++seq,
        filename: file.name,
        size: file.size,
        state: 'uploading',
        progress: 0,
        error: null,
        attachment: null,
        ctrl: null,
      })
      // Use the reactive proxy so progress updates render.
      void start(files[files.length - 1]!, file)
    }
    return accepted.length < incoming.length ? `At most ${MAX_ATTACHMENTS} files per message.` : null
  }

  function remove(localId: number) {
    const i = files.findIndex((f) => f.localId === localId)
    if (i < 0) return
    files[i]?.ctrl?.abort()
    files.splice(i, 1)
  }

  function clear() {
    for (const f of files) f.ctrl?.abort()
    files.splice(0, files.length)
  }

  return {
    files,
    add,
    remove,
    clear,
    uploading: () => files.some((f) => f.state === 'uploading'),
    ids: () => files.filter((f) => f.state === 'ready' && f.attachment).map((f) => f.attachment!.id),
  }
}
