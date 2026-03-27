import { Redis } from "ioredis";
import type {
  Channel, Message, ReadResult, Status, StatusChange, Ack, ThreadResult,
} from "./types.js";

export { type Channel, type Message, type ReadResult, type Status, type StatusChange, type Ack, type ThreadResult };

type StreamEntry = [id: string, fields: string[]];

const CURSOR_RE = /^\d+-\d+$/;
const CHANNEL_NAME_RE = /^[a-zA-Z0-9][a-zA-Z0-9._-]{0,127}$/;
const MAX_CONTENT_LENGTH = 32_768;
const MAX_STATUS_KEY_LENGTH = 256;
const MAX_STATUS_VALUE_LENGTH = 32_768;

export function isValidCursor(id: string): boolean {
  return CURSOR_RE.test(id);
}

export function isValidChannelName(name: string): boolean {
  return CHANNEL_NAME_RE.test(name);
}

export { MAX_CONTENT_LENGTH, MAX_STATUS_KEY_LENGTH, MAX_STATUS_VALUE_LENGTH };

function fieldsToMap(fields: string[]): Record<string, string> {
  const map: Record<string, string> = {};
  for (let i = 0; i < fields.length; i += 2) {
    map[fields[i]] = fields[i + 1];
  }
  return map;
}

function parseMessage(channel: string, entry: StreamEntry): Message {
  const [id, fields] = entry;
  const data = fieldsToMap(fields);
  return {
    id,
    channel,
    sender: data.sender || "unknown",
    content: data.content || "",
    mention: data.mention || null,
    thread_id: data.thread_id || null,
    reply_count: parseInt(data.reply_count || "0", 10),
    created_at: data.created_at || "",
  };
}

function parseStatusChange(channel: string, entry: StreamEntry): StatusChange {
  const [id, fields] = entry;
  const data = fieldsToMap(fields);
  return {
    id,
    channel,
    key: data.key || "",
    value: data.value || "",
    changed_by: data.changed_by || null,
    changed_at: data.changed_at || "",
  };
}

export function createClient(url?: string): Redis {
  return new Redis(url || "redis://localhost:6379", {
    lazyConnect: true,
    maxRetriesPerRequest: 3,
  });
}

/**
 * Team-namespaced relay store. All Redis keys are prefixed with the team ID
 * so multiple teams share the same Redis instance safely.
 */
export class RelayStore {
  constructor(private redis: Redis, private teamId: string) {}

  private k(suffix: string) {
    return `t:${this.teamId}:${suffix}`;
  }

  private async ensureChannel(name: string): Promise<void> {
    const added = await this.redis.sadd(this.k("channels"), name);
    if (added) {
      await this.redis.hsetnx(this.k(`ch:${name}`), "created_at", new Date().toISOString());
    }
  }

  async channelExists(name: string): Promise<boolean> {
    return (await this.redis.sismember(this.k("channels"), name)) === 1;
  }

  async createChannel(name: string, description?: string): Promise<{ channel: Channel; created: boolean }> {
    const added = await this.redis.sadd(this.k("channels"), name);
    const key = this.k(`ch:${name}`);
    await this.redis.hsetnx(key, "created_at", new Date().toISOString());
    if (description) {
      await this.redis.hset(key, "description", description);
    }
    const info = await this.redis.hgetall(key);
    const channel: Channel = {
      name,
      description: info.description || null,
      created_at: info.created_at || new Date().toISOString(),
    };
    return { channel, created: added === 1 };
  }

  async listChannels(): Promise<Channel[]> {
    const names = await this.redis.smembers(this.k("channels"));
    const sorted = names.sort();
    const pipeline = this.redis.pipeline();
    for (const name of sorted) {
      pipeline.hgetall(this.k(`ch:${name}`));
    }
    const results = await pipeline.exec();
    return sorted.map((name: string, i: number) => {
      const info = (results![i][1] as Record<string, string>) || {};
      return {
        name,
        description: info.description || null,
        created_at: info.created_at || "",
      };
    });
  }

  async deleteChannel(name: string): Promise<boolean> {
    const removed = await this.redis.srem(this.k("channels"), name);
    if (!removed) return false;

    const pipeline = this.redis.pipeline();
    pipeline.del(this.k(`ch:${name}`));
    pipeline.del(this.k(`msg:${name}`));
    pipeline.del(this.k(`acks:${name}`));
    pipeline.del(this.k(`status:${name}`));
    pipeline.del(this.k(`slog:${name}`));
    pipeline.del(this.k(`thrc:${name}`));
    await pipeline.exec();

    // Thread reply streams — scan and delete
    const pattern = this.k(`thr:${name}:*`);
    let cursor = "0";
    do {
      const [next, keys] = await this.redis.scan(cursor, "MATCH", pattern, "COUNT", 100);
      cursor = next;
      if (keys.length > 0) {
        await this.redis.del(...keys);
      }
    } while (cursor !== "0");

    return true;
  }

  async messageExists(channel: string, messageId: string): Promise<boolean> {
    const result = await this.redis.xrange(this.k(`msg:${channel}`), messageId, messageId);
    return result.length > 0;
  }

  async postMessage(
    channel: string,
    sender: string,
    content: string,
    mention?: string,
    threadId?: string
  ): Promise<Message> {
    await this.ensureChannel(channel);
    const now = new Date().toISOString();
    const fields: (string | Buffer)[] = [
      "sender", sender, "content", content, "created_at", now,
    ];
    if (mention) fields.push("mention", mention);
    if (threadId) fields.push("thread_id", threadId);

    // Post to main channel stream
    const id = await this.redis.xadd(this.k(`msg:${channel}`), "*", ...fields);

    // If this is a reply, also add to the thread stream and bump reply_count on parent
    if (threadId) {
      const threadKey = this.k(`thr:${channel}:${threadId}`);
      await this.redis.xadd(threadKey, id!, ...fields);
      await this.redis.hincrby(this.k(`thrc:${channel}`), threadId, 1);
    }

    return {
      id: id!,
      channel,
      sender,
      content,
      mention: mention || null,
      thread_id: threadId || null,
      reply_count: 0,
      created_at: now,
    };
  }

  async deleteMessage(channel: string, messageId: string): Promise<boolean> {
    const removed = await this.redis.xdel(this.k(`msg:${channel}`), messageId);
    return removed > 0;
  }

  async readMessages(
    channel: string,
    afterId?: string,
    limit?: number,
    mention?: string,
    sender?: string,
    threadId?: string
  ): Promise<ReadResult> {
    const effectiveLimit = limit ?? 50;
    const streamKey = threadId
      ? this.k(`thr:${channel}:${threadId}`)
      : this.k(`msg:${channel}`);
    let entries: StreamEntry[];

    if (afterId) {
      const result = await this.redis.xread("COUNT", effectiveLimit + 1, "STREAMS", streamKey, afterId);
      entries = result ? (result[0][1] as StreamEntry[]) : [];
    } else {
      const raw = await this.redis.xrevrange(streamKey, "+", "-", "COUNT", effectiveLimit + 1);
      entries = (raw as StreamEntry[]).reverse();
    }

    const hasMore = entries.length > effectiveLimit;
    if (hasMore) entries = entries.slice(0, effectiveLimit);

    let messages = entries.map((e) => parseMessage(channel, e));

    // Enrich top-level messages with reply counts
    if (!threadId) {
      const ids = messages.map((m) => m.id);
      if (ids.length > 0) {
        const pipeline = this.redis.pipeline();
        for (const mid of ids) {
          pipeline.hget(this.k(`thrc:${channel}`), mid);
        }
        const counts = await pipeline.exec();
        for (let i = 0; i < messages.length; i++) {
          const count = counts![i][1];
          if (count) messages[i].reply_count = parseInt(count as string, 10);
        }
      }
    }

    if (mention) {
      messages = messages.filter((m) => m.mention === mention);
    }
    if (sender) {
      messages = messages.filter((m) => m.sender === sender);
    }

    const nextAfterId = messages.length > 0 ? messages[messages.length - 1].id : (afterId ?? "0-0");
    return { messages, next_after_id: nextAfterId, has_more: hasMore, channel };
  }

  async readThread(
    channel: string,
    parentId: string,
    afterId?: string,
    limit?: number
  ): Promise<ThreadResult> {
    // Get parent message from main stream
    const raw = await this.redis.xrange(this.k(`msg:${channel}`), parentId, parentId);
    if (!raw || raw.length === 0) {
      throw new Error("PARENT_NOT_FOUND");
    }
    const parent = parseMessage(channel, raw[0] as StreamEntry);
    const replyCount = await this.redis.hget(this.k(`thrc:${channel}`), parentId);
    parent.reply_count = parseInt(replyCount || "0", 10);

    // Get replies from thread stream
    const effectiveLimit = limit ?? 50;
    const threadKey = this.k(`thr:${channel}:${parentId}`);
    let entries: StreamEntry[];

    if (afterId) {
      const result = await this.redis.xread("COUNT", effectiveLimit + 1, "STREAMS", threadKey, afterId);
      entries = result ? (result[0][1] as StreamEntry[]) : [];
    } else {
      const all = await this.redis.xrange(threadKey, "-", "+", "COUNT", effectiveLimit + 1);
      entries = all as StreamEntry[];
    }

    const hasMore = entries.length > effectiveLimit;
    if (hasMore) entries = entries.slice(0, effectiveLimit);

    const replies = entries.map((e) => parseMessage(channel, e));
    const nextAfterId = replies.length > 0 ? replies[replies.length - 1].id : (afterId ?? "0-0");

    return { parent, replies, next_after_id: nextAfterId, has_more: hasMore };
  }

  async ackMessages(channel: string, sender: string, lastReadId: string): Promise<Ack> {
    const key = this.k(`acks:${channel}`);
    const existing = await this.redis.hget(key, sender);

    let effectiveId = lastReadId;
    if (existing) {
      const prev: Ack = JSON.parse(existing);
      if (prev.last_read_id > lastReadId) effectiveId = prev.last_read_id;
    }

    const now = new Date().toISOString();
    const ack: Ack = { channel, sender, last_read_id: effectiveId, acked_at: now };
    await this.redis.hset(key, sender, JSON.stringify(ack));
    return ack;
  }

  async getAcks(channel: string): Promise<Ack[]> {
    const raw: Record<string, string> = await this.redis.hgetall(this.k(`acks:${channel}`));
    return Object.values(raw)
      .map((v: string) => JSON.parse(v) as Ack)
      .sort((a, b) => a.sender.localeCompare(b.sender));
  }

  async setStatus(channel: string, key: string, value: string, updatedBy?: string): Promise<Status> {
    await this.ensureChannel(channel);
    const now = new Date().toISOString();
    const status: Status = { channel, key, value, updated_by: updatedBy ?? null, updated_at: now };

    const pipeline = this.redis.pipeline();
    pipeline.hset(this.k(`status:${channel}`), key, JSON.stringify(status));
    pipeline.xadd(this.k(`slog:${channel}`), "*",
      "key", key, "value", value, "changed_by", updatedBy ?? "", "changed_at", now);
    await pipeline.exec();

    return status;
  }

  async getStatus(channel?: string, key?: string): Promise<Status[]> {
    if (channel && key) {
      const raw = await this.redis.hget(this.k(`status:${channel}`), key);
      return raw ? [JSON.parse(raw)] : [];
    }
    if (channel) {
      const raw: Record<string, string> = await this.redis.hgetall(this.k(`status:${channel}`));
      return Object.values(raw)
        .map((v: string) => JSON.parse(v) as Status)
        .sort((a, b) => a.key.localeCompare(b.key));
    }

    const channels = await this.redis.smembers(this.k("channels"));
    const results: Status[] = [];

    if (key) {
      const pipeline = this.redis.pipeline();
      for (const ch of channels) pipeline.hget(this.k(`status:${ch}`), key);
      const pipeResults = await pipeline.exec();
      for (let i = 0; i < channels.length; i++) {
        const raw = pipeResults![i][1];
        if (typeof raw === "string") results.push(JSON.parse(raw));
      }
    } else {
      const pipeline = this.redis.pipeline();
      for (const ch of channels) pipeline.hgetall(this.k(`status:${ch}`));
      const pipeResults = await pipeline.exec();
      for (let i = 0; i < channels.length; i++) {
        const raw = pipeResults![i][1];
        if (raw && typeof raw === "object") {
          for (const v of Object.values(raw as Record<string, string>)) {
            results.push(JSON.parse(v));
          }
        }
      }
    }

    return results.sort((a, b) =>
      a.channel === b.channel ? a.key.localeCompare(b.key) : a.channel.localeCompare(b.channel));
  }

  async getStatusChanges(
    channel: string,
    afterId?: string,
    limit?: number
  ): Promise<{ changes: StatusChange[]; next_after_id: string; has_more: boolean }> {
    const effectiveLimit = limit ?? 50;
    const logKey = this.k(`slog:${channel}`);
    let entries: StreamEntry[];

    if (afterId) {
      const result = await this.redis.xread("COUNT", effectiveLimit + 1, "STREAMS", logKey, afterId);
      entries = result ? (result[0][1] as StreamEntry[]) : [];
    } else {
      const raw = await this.redis.xrevrange(logKey, "+", "-", "COUNT", effectiveLimit + 1);
      entries = (raw as StreamEntry[]).reverse();
    }

    const hasMore = entries.length > effectiveLimit;
    if (hasMore) entries = entries.slice(0, effectiveLimit);

    const changes = entries.map((e) => parseStatusChange(channel, e));
    const nextAfterId = changes.length > 0 ? changes[changes.length - 1].id : (afterId ?? "0-0");
    return { changes, next_after_id: nextAfterId, has_more: hasMore };
  }

  // ---- SSE support ----

  async blockingRead(
    channel: string,
    afterId: string,
    timeoutMs: number,
    subscriber: Redis
  ): Promise<Message[]> {
    const streamKey = this.k(`msg:${channel}`);
    const result = await subscriber.xread(
      "COUNT", 100, "BLOCK", timeoutMs, "STREAMS", streamKey, afterId
    );
    if (!result) return [];
    const entries = result[0][1] as StreamEntry[];
    const messages = entries.map((e) => parseMessage(channel, e));
    if (messages.length > 0) {
      const pipeline = this.redis.pipeline();
      for (const m of messages) pipeline.hget(this.k(`thrc:${channel}`), m.id);
      const counts = await pipeline.exec();
      for (let i = 0; i < messages.length; i++) {
        const count = counts![i][1];
        if (count) messages[i].reply_count = parseInt(count as string, 10);
      }
    }
    return messages;
  }

  // ---- Rate limiting ----

  async checkRateLimit(key: string, maxRequests: number, windowMs: number): Promise<{ allowed: boolean; remaining: number }> {
    const now = Date.now();
    const windowKey = `rl:${key}:${Math.floor(now / windowMs)}`;
    const count = await this.redis.incr(windowKey);
    if (count === 1) {
      await this.redis.pexpire(windowKey, windowMs);
    }
    return { allowed: count <= maxRequests, remaining: Math.max(0, maxRequests - count) };
  }
}
