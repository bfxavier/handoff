import { EventEmitter } from "events";
import { webcrypto } from "crypto";
import type {
  Channel, Message, ReadResult, Status, StatusChange, Ack, ThreadResult,
} from "./types.js";

export type {
  Channel, Message, ReadResult, Status, StatusChange, Ack, ThreadResult,
};

export interface HandoffOptions {
  apiUrl: string;
  apiKey: string;
  /** Team-shared encryption key for E2EE. When set, message content and status
   *  values are encrypted client-side with AES-256-GCM before being sent to
   *  the server. The server only ever sees ciphertext. */
  encryptionKey?: string;
}

export interface PostOptions {
  mention?: string;
  thread_id?: string;
}

export interface ReadOptions {
  after_id?: string;
  limit?: number;
  mention?: string;
  sender?: string;
  thread_id?: string;
}

export interface StatusChangesOptions {
  after_id?: string;
  limit?: number;
}

export interface SubscribeOptions {
  last_event_id?: string;
}

export type MessageHandler = (message: Message) => void;
export type ErrorHandler = (error: Error) => void;

// ---- E2EE helpers ----

const E2EE_PREFIX = "e2ee:";
const HKDF_SALT_LEN = 32;
const subtle = (globalThis.crypto?.subtle ?? webcrypto.subtle) as SubtleCrypto;
const rng = globalThis.crypto ?? webcrypto;

async function deriveKey(secret: string, salt: Uint8Array): Promise<CryptoKey> {
  const enc = new TextEncoder();
  // HKDF extract: import the secret as raw key material
  const keyMaterial = await subtle.importKey(
    "raw", enc.encode(secret), "HKDF", false, ["deriveKey"]
  );
  // HKDF expand: derive AES-256-GCM key with domain-specific info
  return subtle.deriveKey(
    { name: "HKDF", hash: "SHA-256", salt, info: enc.encode("handoff-e2ee-v1") },
    keyMaterial,
    { name: "AES-GCM", length: 256 },
    false,
    ["encrypt", "decrypt"]
  );
}

async function encrypt(secret: string, plaintext: string): Promise<string> {
  const enc = new TextEncoder();
  const salt = new Uint8Array(HKDF_SALT_LEN);
  (rng as Crypto).getRandomValues(salt);
  const key = await deriveKey(secret, salt);
  const iv = new Uint8Array(12);
  (rng as Crypto).getRandomValues(iv);
  const ciphertext = await subtle.encrypt(
    { name: "AES-GCM", iv },
    key,
    enc.encode(plaintext)
  );
  // Pack as: base64(salt + iv + ciphertext)
  const packed = new Uint8Array(HKDF_SALT_LEN + iv.length + ciphertext.byteLength);
  packed.set(salt);
  packed.set(iv, HKDF_SALT_LEN);
  packed.set(new Uint8Array(ciphertext), HKDF_SALT_LEN + iv.length);
  return E2EE_PREFIX + Buffer.from(packed).toString("base64");
}

async function decrypt(secret: string, encoded: string): Promise<string> {
  if (!encoded.startsWith(E2EE_PREFIX)) return encoded; // plaintext passthrough
  const packed = new Uint8Array(Buffer.from(encoded.slice(E2EE_PREFIX.length), "base64"));
  const salt = packed.slice(0, HKDF_SALT_LEN);
  const iv = packed.slice(HKDF_SALT_LEN, HKDF_SALT_LEN + 12);
  const ciphertext = packed.slice(HKDF_SALT_LEN + 12);
  const key = await deriveKey(secret, salt);
  const plaintext = await subtle.decrypt(
    { name: "AES-GCM", iv },
    key,
    ciphertext
  );
  return new TextDecoder().decode(plaintext);
}

// ---- Error class ----

export class HandoffError extends Error {
  constructor(
    message: string,
    public readonly status: number,
    public readonly code: string
  ) {
    super(message);
    this.name = "HandoffError";
  }
}

// ---- Subscription (SSE) ----

export class Subscription extends EventEmitter {
  private controller: AbortController;
  private running = false;
  private decryptFn: ((s: string) => Promise<string>) | null;

  constructor(
    private url: string,
    private channel: string,
    decryptFn: ((s: string) => Promise<string>) | null
  ) {
    super();
    this.controller = new AbortController();
    this.decryptFn = decryptFn;
  }

  async connect(): Promise<void> {
    if (this.running) return;
    this.running = true;

    try {
      const res = await fetch(this.url, {
        signal: this.controller.signal,
        headers: { "Accept": "text/event-stream" },
      });

      if (!res.ok || !res.body) {
        this.running = false;
        this.emit("error", new HandoffError(
          `SSE connection failed: ${res.status}`,
          res.status,
          "SSE_CONNECT_FAILED"
        ));
        return;
      }

      const reader = res.body.getReader();
      const decoder = new TextDecoder();
      let buffer = "";
      let currentEvent = "";
      let currentData = "";
      let currentId = "";

      while (this.running) {
        const { done, value } = await reader.read();
        if (done) break;

        buffer += decoder.decode(value, { stream: true });
        const lines = buffer.split("\n");
        buffer = lines.pop() || "";

        for (const line of lines) {
          if (line.startsWith("event: ")) {
            currentEvent = line.slice(7);
          } else if (line.startsWith("data: ")) {
            currentData += line.slice(6);
          } else if (line.startsWith("id: ")) {
            currentId = line.slice(4);
          } else if (line === "") {
            if (currentEvent === "message" && currentData) {
              try {
                const msg: Message = JSON.parse(currentData);
                if (this.decryptFn) {
                  msg.content = await this.decryptFn(msg.content);
                }
                this.emit("message", msg);
              } catch {
                // Skip malformed events
              }
            } else if (currentEvent === "error" && currentData) {
              try {
                const err = JSON.parse(currentData);
                this.emit("error", new HandoffError(err.error || "Stream error", 0, "SSE_ERROR"));
              } catch { /* skip */ }
            }
            currentEvent = "";
            currentData = "";
            if (currentId) {
              this.emit("cursor", currentId);
              currentId = "";
            }
          }
        }
      }
    } catch (err) {
      if (this.running) {
        this.emit("error", err instanceof Error ? err : new Error(String(err)));
      }
    } finally {
      this.running = false;
      this.emit("close");
    }
  }

  get connected(): boolean {
    return this.running;
  }

  close(): void {
    this.running = false;
    this.controller.abort();
  }

  on(event: "message", listener: MessageHandler): this;
  on(event: "error", listener: ErrorHandler): this;
  on(event: "cursor", listener: (id: string) => void): this;
  on(event: "close", listener: () => void): this;
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  on(event: string, listener: (...args: any[]) => void): this {
    return super.on(event, listener);
  }
}

// ---- Main client ----

export class Handoff {
  private apiUrl: string;
  private apiKey: string;
  private encryptionKey: string | null = null;

  /** True if E2EE is enabled */
  readonly encrypted: boolean;

  constructor(options: HandoffOptions) {
    this.apiUrl = options.apiUrl.replace(/\/$/, "");
    this.apiKey = options.apiKey;
    this.encrypted = !!options.encryptionKey;
    this.encryptionKey = options.encryptionKey ?? null;
  }

  private async encryptContent(plaintext: string): Promise<string> {
    if (!this.encrypted || !this.encryptionKey) return plaintext;
    return encrypt(this.encryptionKey, plaintext);
  }

  private async decryptContent(ciphertext: string): Promise<string> {
    if (!this.encrypted || !this.encryptionKey) return ciphertext;
    return decrypt(this.encryptionKey, ciphertext);
  }

  private async decryptMessage(msg: Message): Promise<Message> {
    if (!this.encrypted) return msg;
    return { ...msg, content: await this.decryptContent(msg.content) };
  }

  private async decryptMessages(msgs: Message[]): Promise<Message[]> {
    if (!this.encrypted) return msgs;
    return Promise.all(msgs.map(m => this.decryptMessage(m)));
  }

  private async decryptStatus(st: Status): Promise<Status> {
    if (!this.encrypted) return st;
    return { ...st, value: await this.decryptContent(st.value) };
  }

  private async decryptStatuses(statuses: Status[]): Promise<Status[]> {
    if (!this.encrypted) return statuses;
    return Promise.all(statuses.map(s => this.decryptStatus(s)));
  }

  private async decryptStatusChange(sc: StatusChange): Promise<StatusChange> {
    if (!this.encrypted) return sc;
    return { ...sc, value: await this.decryptContent(sc.value) };
  }

  private async request<T>(method: string, path: string, body?: unknown): Promise<T> {
    const res = await fetch(`${this.apiUrl}${path}`, {
      method,
      headers: {
        "Content-Type": "application/json",
        "Authorization": `Bearer ${this.apiKey}`,
      },
      body: body ? JSON.stringify(body) : undefined,
    });

    if (res.status === 204) return undefined as T;

    const data = await res.json();

    if (!res.ok) {
      throw new HandoffError(
        data.error || `HTTP ${res.status}`,
        res.status,
        data.code || "UNKNOWN"
      );
    }

    return data as T;
  }

  // ---- Channels ----

  async createChannel(name: string, description?: string): Promise<Channel> {
    return this.request("POST", "/api/channels", { name, description });
  }

  async listChannels(): Promise<Channel[]> {
    return this.request("GET", "/api/channels");
  }

  async deleteChannel(name: string): Promise<void> {
    return this.request("DELETE", `/api/channels/${encodeURIComponent(name)}`);
  }

  // ---- Messages ----

  async post(channel: string, content: string, options?: PostOptions): Promise<Message> {
    const encrypted = await this.encryptContent(content);
    const msg = await this.request<Message>("POST", `/api/channels/${encodeURIComponent(channel)}/messages`, {
      content: encrypted,
      mention: options?.mention,
      thread_id: options?.thread_id,
    });
    // Return with original plaintext content, not the ciphertext
    return { ...msg, content };
  }

  async read(channel: string, options?: ReadOptions): Promise<ReadResult> {
    const params = new URLSearchParams();
    if (options?.after_id) params.set("after_id", options.after_id);
    if (options?.limit !== undefined) params.set("limit", String(options.limit));
    if (options?.mention) params.set("mention", options.mention);
    if (options?.sender) params.set("sender", options.sender);
    if (options?.thread_id) params.set("thread_id", options.thread_id);
    const qs = params.size > 0 ? `?${params}` : "";
    const result = await this.request<ReadResult>("GET", `/api/channels/${encodeURIComponent(channel)}/messages${qs}`);
    result.messages = await this.decryptMessages(result.messages);
    return result;
  }

  async deleteMessage(channel: string, messageId: string): Promise<void> {
    return this.request("DELETE", `/api/channels/${encodeURIComponent(channel)}/messages/${encodeURIComponent(messageId)}`);
  }

  // ---- Threads ----

  async reply(channel: string, threadId: string, content: string, options?: Omit<PostOptions, "thread_id">): Promise<Message> {
    return this.post(channel, content, { ...options, thread_id: threadId });
  }

  async thread(channel: string, parentId: string, options?: { after_id?: string; limit?: number }): Promise<ThreadResult> {
    const params = new URLSearchParams();
    if (options?.after_id) params.set("after_id", options.after_id);
    if (options?.limit !== undefined) params.set("limit", String(options.limit));
    const qs = params.size > 0 ? `?${params}` : "";
    const result = await this.request<ThreadResult>("GET", `/api/channels/${encodeURIComponent(channel)}/threads/${encodeURIComponent(parentId)}${qs}`);
    result.parent = await this.decryptMessage(result.parent);
    result.replies = await this.decryptMessages(result.replies);
    return result;
  }

  // ---- Subscriptions (SSE) ----

  subscribe(channel: string, options?: SubscribeOptions): Subscription {
    const params = new URLSearchParams();
    params.set("token", this.apiKey);
    if (options?.last_event_id) params.set("last_event_id", options.last_event_id);
    const url = `${this.apiUrl}/api/channels/${encodeURIComponent(channel)}/stream?${params}`;
    const decryptFn = this.encrypted
      ? (s: string) => this.decryptContent(s)
      : null;
    return new Subscription(url, channel, decryptFn);
  }

  on(channel: string, handler: MessageHandler, options?: SubscribeOptions): () => void {
    const sub = this.subscribe(channel, options);
    sub.on("message", handler);
    sub.connect();
    return () => sub.close();
  }

  // ---- Acks ----

  async ack(channel: string, lastReadId: string): Promise<Ack> {
    return this.request("POST", `/api/channels/${encodeURIComponent(channel)}/ack`, {
      last_read_id: lastReadId,
    });
  }

  async getAcks(channel: string): Promise<Ack[]> {
    return this.request("GET", `/api/channels/${encodeURIComponent(channel)}/acks`);
  }

  // ---- Status ----

  async setStatus(channel: string, key: string, value: string): Promise<Status> {
    const encrypted = await this.encryptContent(value);
    const st = await this.request<Status>("PUT", `/api/channels/${encodeURIComponent(channel)}/status`, { key, value: encrypted });
    return { ...st, value };
  }

  async getStatus(options?: { channel?: string; key?: string }): Promise<Status[]> {
    let statuses: Status[];
    if (options?.channel) {
      const params = new URLSearchParams();
      if (options.key) params.set("key", options.key);
      const qs = params.size > 0 ? `?${params}` : "";
      statuses = await this.request("GET", `/api/channels/${encodeURIComponent(options.channel)}/status${qs}`);
    } else {
      const params = new URLSearchParams();
      if (options?.key) params.set("key", options.key);
      const qs = params.size > 0 ? `?${params}` : "";
      statuses = await this.request("GET", `/api/status${qs}`);
    }
    return this.decryptStatuses(statuses);
  }

  async getStatusChanges(channel: string, options?: StatusChangesOptions): Promise<{ changes: StatusChange[]; next_after_id: string; has_more: boolean }> {
    const params = new URLSearchParams();
    if (options?.after_id) params.set("after_id", options.after_id);
    if (options?.limit !== undefined) params.set("limit", String(options.limit));
    const qs = params.size > 0 ? `?${params}` : "";
    const result = await this.request<{ changes: StatusChange[]; next_after_id: string; has_more: boolean }>(
      "GET", `/api/channels/${encodeURIComponent(channel)}/status/changes${qs}`
    );
    result.changes = await Promise.all(result.changes.map(c => this.decryptStatusChange(c)));
    return result;
  }
}

export default Handoff;
