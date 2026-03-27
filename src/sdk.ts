import { EventEmitter } from "events";
import type {
  Channel, Message, ReadResult, Status, StatusChange, Ack, ThreadResult,
} from "./types.js";

export type {
  Channel, Message, ReadResult, Status, StatusChange, Ack, ThreadResult,
};

export interface HandoffOptions {
  apiUrl: string;
  apiKey: string;
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

export class Subscription extends EventEmitter {
  private controller: AbortController;
  private running = false;

  constructor(
    private url: string,
    private channel: string
  ) {
    super();
    this.controller = new AbortController();
  }

  /** Start the SSE connection. Call .close() to stop. */
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
            // End of event
            if (currentEvent === "message" && currentData) {
              try {
                const msg: Message = JSON.parse(currentData);
                this.emit("message", msg);
              } catch {
                // Skip malformed events
              }
            } else if (currentEvent === "error" && currentData) {
              try {
                const err = JSON.parse(currentData);
                this.emit("error", new HandoffError(err.error || "Stream error", 0, "SSE_ERROR"));
              } catch {
                // Skip
              }
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

  /** True if the SSE connection is active */
  get connected(): boolean {
    return this.running;
  }

  /** Disconnect the SSE stream */
  close(): void {
    this.running = false;
    this.controller.abort();
  }

  // Typed event overloads
  on(event: "message", listener: MessageHandler): this;
  on(event: "error", listener: ErrorHandler): this;
  on(event: "cursor", listener: (id: string) => void): this;
  on(event: "close", listener: () => void): this;
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  on(event: string, listener: (...args: any[]) => void): this {
    return super.on(event, listener);
  }
}

export class Handoff {
  private apiUrl: string;
  private apiKey: string;

  constructor(options: HandoffOptions) {
    this.apiUrl = options.apiUrl.replace(/\/$/, "");
    this.apiKey = options.apiKey;
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
    return this.request("POST", `/api/channels/${encodeURIComponent(channel)}/messages`, {
      content,
      mention: options?.mention,
      thread_id: options?.thread_id,
    });
  }

  async read(channel: string, options?: ReadOptions): Promise<ReadResult> {
    const params = new URLSearchParams();
    if (options?.after_id) params.set("after_id", options.after_id);
    if (options?.limit !== undefined) params.set("limit", String(options.limit));
    if (options?.mention) params.set("mention", options.mention);
    if (options?.sender) params.set("sender", options.sender);
    if (options?.thread_id) params.set("thread_id", options.thread_id);
    const qs = params.size > 0 ? `?${params}` : "";
    return this.request("GET", `/api/channels/${encodeURIComponent(channel)}/messages${qs}`);
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
    return this.request("GET", `/api/channels/${encodeURIComponent(channel)}/threads/${encodeURIComponent(parentId)}${qs}`);
  }

  // ---- Subscriptions (SSE) ----

  subscribe(channel: string, options?: SubscribeOptions): Subscription {
    const params = new URLSearchParams();
    params.set("token", this.apiKey);
    if (options?.last_event_id) params.set("last_event_id", options.last_event_id);
    const url = `${this.apiUrl}/api/channels/${encodeURIComponent(channel)}/stream?${params}`;
    return new Subscription(url, channel);
  }

  /**
   * Convenience: subscribe and call handler for each message.
   * Returns a function to unsubscribe.
   */
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
    return this.request("PUT", `/api/channels/${encodeURIComponent(channel)}/status`, { key, value });
  }

  async getStatus(options?: { channel?: string; key?: string }): Promise<Status[]> {
    if (options?.channel) {
      const params = new URLSearchParams();
      if (options.key) params.set("key", options.key);
      const qs = params.size > 0 ? `?${params}` : "";
      return this.request("GET", `/api/channels/${encodeURIComponent(options.channel)}/status${qs}`);
    }
    const params = new URLSearchParams();
    if (options?.key) params.set("key", options.key);
    const qs = params.size > 0 ? `?${params}` : "";
    return this.request("GET", `/api/status${qs}`);
  }

  async getStatusChanges(channel: string, options?: StatusChangesOptions): Promise<{ changes: StatusChange[]; next_after_id: string; has_more: boolean }> {
    const params = new URLSearchParams();
    if (options?.after_id) params.set("after_id", options.after_id);
    if (options?.limit !== undefined) params.set("limit", String(options.limit));
    const qs = params.size > 0 ? `?${params}` : "";
    return this.request("GET", `/api/channels/${encodeURIComponent(channel)}/status/changes${qs}`);
  }
}

export default Handoff;
