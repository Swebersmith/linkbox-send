import { api, ApiError, getCSRF, message, type Settings } from './api'

type Status = 'queued' | 'uploading' | 'paused' | 'needs-file' | 'merging' | 'complete' | 'error' | 'canceling'
export interface UploadState { uploadId: string; fileName: string; fileSize: number; chunkSize: number; totalChunks: number; uploadedChunks: number[]; status: 'uploading' | 'merging' | 'complete'; fileId: string; error?: string }
export interface Task {
  key: string; uploadId?: string; name: string; size: number; lastModified: number; chunkSize: number; fileId?: string;
  status: Status; uploaded: number; speed: number; averageSpeed: number; failedChunks: number[]; error?: string;
  file?: File; stopped?: boolean; controllers: Set<XMLHttpRequest>; inFlight: Map<number, number>;
  acknowledged: number; started: number; startingBytes: number; lastBytes: number; lastTime: number;
}
const sleep = (ms: number) => new Promise<void>(resolve => setTimeout(resolve, ms))
const stopped = () => new DOMException('Upload paused', 'AbortError')
const savedStatuses = new Set<Status>(['merging', 'complete', 'paused', 'needs-file', 'error', 'uploading', 'queued'])

export class UploadQueue {
  private tasks: Task[] = []
  private listeners = new Set<() => void>()
  private snapshot: Task[] = []
  private busy = false
  private disposed = false
  private timer: ReturnType<typeof setInterval>
  private storageKey: string
  concurrency: number
  persistenceError = ''
  constructor(private settings: Settings, userId: string) {
    this.storageKey = `linkbox-uploads:${userId}`
    this.concurrency = settings.uploadConcurrency
    try {
      const saved = JSON.parse(localStorage.getItem(this.storageKey) || '[]') as Partial<Task>[]
      this.tasks = Array.isArray(saved) ? saved.filter(t => t.uploadId && t.name && typeof t.size === 'number' && savedStatuses.has(t.status as Status)).slice(0, 200).map(t => this.makeTask({ ...t, status: t.status === 'complete' ? 'complete' : t.status === 'merging' ? 'merging' : 'needs-file' })) : []
    } catch { this.persistenceError = '浏览器任务记录不可用，请保持此页面打开。' }
    this.emit()
    this.timer = setInterval(() => this.measure(), 500)
    void this.reconcile()
  }
  subscribe = (fn: () => void) => { this.listeners.add(fn); return () => this.listeners.delete(fn) }
  getSnapshot = () => this.snapshot
  private makeTask(v: Partial<Task>): Task { return { key: crypto.randomUUID(), name: '', size: 0, lastModified: 0, chunkSize: this.settings.chunkSize, status: 'queued', uploaded: 0, speed: 0, averageSpeed: 0, failedChunks: [], acknowledged: 0, started: 0, startingBytes: 0, lastBytes: 0, lastTime: 0, ...v, controllers: new Set(), inFlight: new Map() } }
  private emit() {
    this.snapshot = this.tasks.map(t => ({ ...t, failedChunks: [...t.failedChunks] }))
    try {
      localStorage.setItem(this.storageKey, JSON.stringify(this.tasks.filter(t => t.uploadId).map(({ key, uploadId, name, size, lastModified, chunkSize, fileId, status }) => ({ key, uploadId, name, size, lastModified, chunkSize, fileId, status }))))
    } catch { this.persistenceError = '无法保存断点记录，请检查浏览器存储空间。' }
    this.listeners.forEach(fn => fn())
  }
  private measure() {
    const now = performance.now(); let changed = false
    for (const t of this.tasks) if (t.status === 'uploading') {
      t.uploaded = Math.min(t.size, t.acknowledged + [...t.inFlight.values()].reduce((a, b) => a + b, 0))
      t.speed = Math.max(0, (t.uploaded - t.lastBytes) / Math.max((now - t.lastTime) / 1000, 0.1))
      t.averageSpeed = Math.max(0, (t.uploaded - t.startingBytes) / Math.max((now - t.started) / 1000, 0.1))
      t.lastBytes = t.uploaded; t.lastTime = now; changed = true
    }
    if (changed) this.emit()
  }
  setConcurrency(n: number) { if ([1, 2, 4, 6, 8].includes(n)) { this.concurrency = n; this.emit() } }
  async add(files: File[]) {
    for (const file of files) {
      if (file.size > this.settings.maxFileSize) throw new Error(`${file.name} 超过服务器文件大小上限`)
      let existing = this.tasks.find(t => t.name === file.name && t.size === file.size && t.lastModified === file.lastModified && t.status !== 'complete')
      if (existing) { existing.file = file; if (['needs-file', 'error', 'paused'].includes(existing.status)) { existing.status = 'queued'; existing.error = undefined; if (this.activeKey !== existing.key) existing.stopped = false } }
      else { existing = this.makeTask({ name: file.name, size: file.size, lastModified: file.lastModified, file }); this.tasks.push(existing) }
    }
    this.emit(); void this.pump()
  }
  pause(key: string) { const t = this.tasks.find(t => t.key === key); if (!t || !['uploading', 'queued'].includes(t.status)) return; t.stopped = true; t.status = 'paused'; t.controllers.forEach(x => x.abort()); this.emit() }
  resume(key: string) { const t = this.tasks.find(t => t.key === key); if (!t) return; if (!t.file) { t.status = 'needs-file'; this.emit(); return }; if (this.activeKey !== key) t.stopped = false; t.error = undefined; t.failedChunks = []; t.status = 'queued'; this.emit(); void this.pump() }
  async cancel(key: string) {
    const t = this.tasks.find(t => t.key === key); if (!t || t.status === 'merging') return
    t.stopped = true; t.status = 'canceling'; t.controllers.forEach(x => x.abort()); this.emit()
    // Wait for initialization and all in-flight requests to settle before deleting the upload.
    while (this.busy && this.activeKey === key) await sleep(100)
    try { if (t.uploadId) await api(`/uploads/${t.uploadId}`, { method: 'DELETE' }); this.tasks = this.tasks.filter(v => v.key !== key) }
    catch (e) { t.status = 'error'; t.error = message(e) }
    this.emit()
  }
  dismiss(key: string) { this.tasks = this.tasks.filter(t => t.key !== key || t.status !== 'complete'); this.emit() }
  dispose() { this.disposed = true; clearInterval(this.timer); this.tasks.forEach(t => { t.stopped = true; t.controllers.forEach(x => x.abort()) }); this.listeners.clear() }
  private activeKey?: string
  private async reconcile() {
    for (const t of this.tasks) {
      if (t.status === 'complete' || !t.uploadId || this.disposed) continue
      try {
        const state = await api<UploadState>(`/uploads/${t.uploadId}`)
        if (t.file || t.status === 'canceling') continue
        this.sync(t, state)
        if (state.status === 'merging') { t.status = 'merging'; void this.poll(t) }
        else if (state.status !== 'complete') { t.status = 'needs-file'; t.error = state.error }
      } catch (e) { t.status = 'error'; t.error = message(e) }
      this.emit()
    }
  }
  private sync(t: Task, state: UploadState) {
    t.chunkSize = state.chunkSize; t.fileId = state.fileId
    t.acknowledged = state.uploadedChunks.reduce((n, i) => n + Math.min(state.chunkSize, t.size - i * state.chunkSize), 0)
    t.uploaded = t.acknowledged
    if (state.status === 'complete') { t.status = 'complete'; t.uploaded = t.size; t.file = undefined }
  }
  private async pump() {
    if (this.busy || this.disposed) return
    const t = this.tasks.find(v => v.status === 'queued' && v.file)
    if (!t) return
    this.busy = true; this.activeKey = t.key
    try { await this.run(t) }
    catch (e) { if (!t.stopped && !this.disposed) { t.status = 'error'; t.error = message(e) } }
    finally { t.inFlight.clear(); t.speed = 0; this.busy = false; this.activeKey = undefined; this.emit(); void this.pump() }
  }
  private async run(t: Task) {
    t.stopped = false
    t.status = 'uploading'; this.emit()
    let state: UploadState
    if (t.uploadId) state = await api<UploadState>(`/uploads/${t.uploadId}`)
    else {
      state = await api<UploadState>('/uploads', { method: 'POST', body: JSON.stringify({ name: t.name, size: t.size, mimeType: t.file!.type || 'application/octet-stream', chunkSize: this.settings.chunkSize }) })
      t.uploadId = state.uploadId; this.emit()
    }
    this.sync(t, state)
    if (state.status === 'complete') return
    if (t.stopped || this.disposed) throw stopped()
    if (state.status === 'merging') { t.status = 'merging'; await this.poll(t); return }
    const uploaded = new Set(state.uploadedChunks)
    const missing = Array.from({ length: state.totalChunks }, (_, i) => i).filter(i => !uploaded.has(i))
    t.started = t.lastTime = performance.now(); t.startingBytes = t.lastBytes = t.acknowledged; t.failedChunks = []
    // One file at a time, multiple chunks per file. The next batch uses the current concurrency setting.
    while (missing.length) {
      if (t.stopped || this.disposed) throw stopped()
      const batch = missing.splice(0, this.concurrency)
      const results = await Promise.allSettled(batch.map(i => this.sendChunk(t, i)))
      const failed = results.find(v => v.status === 'rejected')
      if (failed?.status === 'rejected') throw failed.reason
    }
    if (t.stopped || this.disposed) throw stopped()
    t.status = 'merging'; t.uploaded = t.size; this.emit()
    await api(`/uploads/${t.uploadId}/complete`, { method: 'POST' })
    await this.poll(t)
  }
  private async poll(t: Task) {
    let errors = 0
    while (!this.disposed) {
      try {
        const state = await api<UploadState>(`/uploads/${t.uploadId}`)
        this.sync(t, state); errors = 0
        if (state.status === 'complete') { this.emit(); return }
        if (state.status === 'uploading') throw new ApiError(409, state.error || '合并未完成，请重试')
      } catch (e) {
        if (e instanceof ApiError || ++errors > 5) { t.status = 'error'; t.error = message(e); this.emit(); return }
      }
      await sleep(1500)
    }
  }
  private async sendChunk(t: Task, index: number) {
    const blob = t.file!.slice(index * t.chunkSize, Math.min(t.size, (index + 1) * t.chunkSize))
    const digest = await crypto.subtle.digest('SHA-256', await blob.arrayBuffer())
    const sha = [...new Uint8Array(digest)].map(v => v.toString(16).padStart(2, '0')).join('')
    for (let attempt = 0; attempt <= 3; attempt++) {
      if (t.stopped || this.disposed) throw stopped()
      try { await this.xhr(t, index, blob, sha); t.inFlight.delete(index); t.acknowledged += blob.size; t.uploaded = t.acknowledged; return }
      catch (e) {
        t.inFlight.delete(index)
        if (t.stopped || this.disposed) throw stopped()
        const retryable = !(e instanceof ApiError) || [408, 425, 429, 500, 502, 503, 504, 520, 522, 524].includes(e.status)
        if (attempt === 3 || !retryable) { t.failedChunks.push(index); throw e }
        await sleep(1000 * 2 ** attempt)
      }
    }
  }
  private xhr(t: Task, index: number, blob: Blob, sha: string) {
    return new Promise<void>((resolve, reject) => {
      const xhr = new XMLHttpRequest(); t.controllers.add(xhr)
      const cleanup = () => t.controllers.delete(xhr)
      xhr.open('PUT', `/api/uploads/${t.uploadId}/chunks/${index}`)
      xhr.withCredentials = true; xhr.timeout = 30 * 60 * 1000
      xhr.setRequestHeader('Content-Type', 'application/octet-stream'); xhr.setRequestHeader('X-CSRF-Token', getCSRF()); xhr.setRequestHeader('X-Chunk-SHA256', sha)
      xhr.upload.onprogress = e => { t.inFlight.set(index, e.loaded) }
      xhr.onload = () => { cleanup(); if (xhr.status >= 200 && xhr.status < 300) resolve(); else { let body; try { body = JSON.parse(xhr.responseText) } catch { body = {} }; reject(new ApiError(xhr.status, body.error || `分片 ${index} 失败 (${xhr.status})`)) } }
      xhr.onerror = () => { cleanup(); reject(new Error('网络连接中断')) }
      xhr.ontimeout = () => { cleanup(); reject(new Error('分片上传超时')) }
      xhr.onabort = () => { cleanup(); reject(stopped()) }
      xhr.send(blob)
    })
  }
}
