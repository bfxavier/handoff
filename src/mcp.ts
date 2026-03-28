#!/usr/bin/env node
import { McpServer } from "@modelcontextprotocol/sdk/server/mcp.js";
import { StdioServerTransport } from "@modelcontextprotocol/sdk/server/stdio.js";
import { z } from "zod";

const API_URL = process.env.RELAY_API_URL?.replace(/\/$/, "");
const API_KEY = process.env.RELAY_API_KEY;

if (!API_URL || !API_KEY) {
  process.stderr.write(
    "Error: RELAY_API_URL and RELAY_API_KEY environment variables are required.\n"
  );
  process.exit(1);
}

async function api(method: string, path: string, body?: unknown): Promise<unknown> {
  const url = `${API_URL}${path}`;
  const res = await fetch(url, {
    method,
    headers: {
      "Content-Type": "application/json",
      "Authorization": `Bearer ${API_KEY}`,
    },
    body: body ? JSON.stringify(body) : undefined,
  });
  if (!res.ok) {
    const text = await res.text();
    throw new Error(`API error ${res.status}: ${text}`);
  }
  if (res.status === 204) return { ok: true };
  return res.json();
}

function ok(result: unknown) {
  return { content: [{ type: "text" as const, text: JSON.stringify(result, null, 2) }] };
}

function err(e: unknown) {
  const message = e instanceof Error ? e.message : String(e);
  return { content: [{ type: "text" as const, text: `Error: ${message}` }] };
}

const server = new McpServer({ name: "agent-relay", version: "0.4.0" });

server.registerTool(
  "create_channel",
  {
    description:
      "Create a named channel for agent-to-agent communication. Channels are the primary grouping for messages, acks, and status. " +
      "Use a short, slug-style name (e.g. 'build-pipeline', 'review-queue'). " +
      "Creating a channel that already exists is a no-op — it returns the existing channel.",
    inputSchema: {
      name: z.string().describe("Channel name (slug-style, e.g. 'build-pipeline')"),
      description: z.string().optional().describe("Human-readable description of the channel's purpose"),
    },
  },
  async (input) => {
    try {
      const result = await api("POST", "/api/channels", {
        name: input.name,
        description: input.description,
      });
      return ok(result);
    } catch (e) {
      return err(e);
    }
  }
);

server.registerTool(
  "list_channels",
  {
    description:
      "List all channels available to your team. Returns channel names, descriptions, and creation timestamps. " +
      "Use this to discover active coordination channels before posting or reading messages.",
    inputSchema: {},
  },
  async () => {
    try {
      const result = await api("GET", "/api/channels");
      return ok(result);
    } catch (e) {
      return err(e);
    }
  }
);

server.registerTool(
  "post_message",
  {
    description:
      "Post a message to a channel. The channel is auto-created if it doesn't exist. " +
      "IMPORTANT: Always set 'mention' when directing a message at a specific agent — this is how they filter for messages meant for them. " +
      "Use 'thread_id' to reply to a specific message, creating a threaded conversation. " +
      "After reading messages, call 'ack' with the last message ID so other agents know you've seen them. " +
      "Your sender identity is derived from your API key on the server side.",
    inputSchema: {
      channel: z.string().describe("Channel name to post to"),
      content: z.string().describe("Message content"),
      mention: z.string().optional().describe("Sender name of the agent this message is directed at"),
      thread_id: z.string().optional().describe("Message ID to reply to (creates a thread). Get this from a read_messages response."),
    },
  },
  async (input) => {
    try {
      const result = await api("POST", `/api/channels/${encodeURIComponent(input.channel)}/messages`, {
        content: input.content,
        mention: input.mention,
        thread_id: input.thread_id,
      });
      return ok(result);
    } catch (e) {
      return err(e);
    }
  }
);

server.registerTool(
  "read_messages",
  {
    description:
      "Read messages from a channel. Returns a cursor (next_after_id) and has_more flag for efficient polling — " +
      "pass the returned next_after_id as after_id on the next call to receive only new messages. " +
      "Omit after_id to get the most recent messages. " +
      "Use 'mention' to filter for messages directed at you. " +
      "Use 'sender' to filter by who sent the message. " +
      "Messages include thread_id (if a reply) and reply_count (number of threaded replies). " +
      "IMPORTANT: After reading, call 'ack' with the last message ID to signal you've read them. " +
      "Tip: Use 'read_unread' instead to get only messages you haven't acked yet. " +
      "Default limit is 50; maximum is 100.",
    inputSchema: {
      channel: z.string().describe("Channel name to read from"),
      after_id: z.string().optional().describe("Cursor from a previous read_messages call; returns only messages after this ID"),
      limit: z.number().int().min(1).max(100).optional().describe("Maximum number of messages to return (default 50)"),
      mention: z.string().optional().describe("Filter messages to only those mentioning this sender name"),
      sender: z.string().optional().describe("Filter messages to only those sent by this sender name"),
    },
  },
  async (input) => {
    try {
      const params = new URLSearchParams();
      if (input.after_id) params.set("after_id", input.after_id);
      if (input.limit !== undefined) params.set("limit", String(input.limit));
      if (input.mention) params.set("mention", input.mention);
      if (input.sender) params.set("sender", input.sender);
      const qs = params.size > 0 ? `?${params}` : "";
      const result = await api("GET", `/api/channels/${encodeURIComponent(input.channel)}/messages${qs}`);
      return ok(result);
    } catch (e) {
      return err(e);
    }
  }
);

server.registerTool(
  "read_thread",
  {
    description:
      "Read a message thread — the parent message and all its replies. " +
      "Returns the parent message with reply_count and a paginated list of replies. " +
      "Use after_id for pagination through long threads.",
    inputSchema: {
      channel: z.string().describe("Channel name"),
      thread_id: z.string().describe("The message ID of the thread parent (from a read_messages response)"),
      after_id: z.string().optional().describe("Cursor for paginating through replies"),
      limit: z.number().int().min(1).max(100).optional().describe("Maximum number of replies to return (default 50)"),
    },
  },
  async (input) => {
    try {
      const params = new URLSearchParams();
      if (input.after_id) params.set("after_id", input.after_id);
      if (input.limit !== undefined) params.set("limit", String(input.limit));
      const qs = params.size > 0 ? `?${params}` : "";
      const result = await api(
        "GET",
        `/api/channels/${encodeURIComponent(input.channel)}/threads/${encodeURIComponent(input.thread_id)}${qs}`
      );
      return ok(result);
    } catch (e) {
      return err(e);
    }
  }
);

server.registerTool(
  "ack",
  {
    description:
      "Acknowledge messages up to and including last_read_id in a channel. " +
      "Acks are per-sender and monotonic — the server will not move an ack backward if a lower ID is submitted. " +
      "Use get_acks to see what all agents have read. Your sender identity comes from your API key.",
    inputSchema: {
      channel: z.string().describe("Channel name to acknowledge"),
      last_read_id: z.string().describe("The ID of the last message you have read (from a read_messages response)"),
    },
  },
  async (input) => {
    try {
      const result = await api("POST", `/api/channels/${encodeURIComponent(input.channel)}/ack`, {
        last_read_id: input.last_read_id,
      });
      return ok(result);
    } catch (e) {
      return err(e);
    }
  }
);

server.registerTool(
  "get_acks",
  {
    description:
      "Get the read acknowledgements for all agents in a channel. " +
      "Each entry shows the sender name and the ID of the last message they acknowledged. " +
      "Useful for checking whether another agent has caught up before proceeding.",
    inputSchema: {
      channel: z.string().describe("Channel name to retrieve acks for"),
    },
  },
  async (input) => {
    try {
      const result = await api("GET", `/api/channels/${encodeURIComponent(input.channel)}/acks`);
      return ok(result);
    } catch (e) {
      return err(e);
    }
  }
);

server.registerTool(
  "read_unread",
  {
    description:
      "Read only messages you haven't acknowledged yet. Uses your ack watermark as the cursor — " +
      "returns messages posted after your last 'ack' call. This is the recommended way to check for new messages. " +
      "After reading, call 'ack' with the last message ID.",
    inputSchema: {
      channel: z.string().describe("Channel name to check for unread messages"),
      limit: z.number().int().min(1).max(100).optional().describe("Maximum number of messages to return (default 50)"),
    },
  },
  async (input) => {
    try {
      const params = new URLSearchParams();
      if (input.limit !== undefined) params.set("limit", String(input.limit));
      const qs = params.size > 0 ? `?${params}` : "";
      const result = await api("GET", `/api/channels/${encodeURIComponent(input.channel)}/unread${qs}`);
      return ok(result);
    } catch (e) {
      return err(e);
    }
  }
);

server.registerTool(
  "set_status",
  {
    description:
      "Set a key/value status entry on a channel. Status entries represent shared state between agents " +
      "(e.g. 'stage' = 'building', 'lock' = 'agent-1'). " +
      "Each write is appended to the status change log, queryable via get_status_changes. " +
      "Values are strings; serialize structured data as JSON if needed.",
    inputSchema: {
      channel: z.string().describe("Channel name to set status on"),
      key: z.string().describe("Status key (e.g. 'stage', 'lock', 'progress')"),
      value: z.string().describe("Status value (string; use JSON for structured data)"),
    },
  },
  async (input) => {
    try {
      const result = await api("PUT", `/api/channels/${encodeURIComponent(input.channel)}/status`, {
        key: input.key,
        value: input.value,
      });
      return ok(result);
    } catch (e) {
      return err(e);
    }
  }
);

server.registerTool(
  "get_status",
  {
    description:
      "Get status entries. Supports two modes:\n" +
      "- Channel-scoped: provide 'channel' (and optionally 'key') to get status for one channel.\n" +
      "- Cross-channel: omit 'channel' and provide 'key' (or neither) to query across all channels.\n" +
      "Returns an array of status entries with channel, key, value, updated_by, and updated_at.",
    inputSchema: {
      channel: z.string().optional().describe("Channel name; omit to query across all channels"),
      key: z.string().optional().describe("Status key to filter by; omit to return all keys"),
    },
  },
  async (input) => {
    try {
      let result: unknown;
      if (input.channel) {
        const params = new URLSearchParams();
        if (input.key) params.set("key", input.key);
        const qs = params.size > 0 ? `?${params}` : "";
        result = await api("GET", `/api/channels/${encodeURIComponent(input.channel)}/status${qs}`);
      } else {
        const params = new URLSearchParams();
        if (input.key) params.set("key", input.key);
        const qs = params.size > 0 ? `?${params}` : "";
        result = await api("GET", `/api/status${qs}`);
      }
      return ok(result);
    } catch (e) {
      return err(e);
    }
  }
);

server.registerTool(
  "get_status_changes",
  {
    description:
      "Read the status change log for a channel. Returns a cursor (next_after_id) for polling — " +
      "pass it as after_id on the next call to receive only new changes. " +
      "Useful for tracking when and by whom status entries were updated over time.",
    inputSchema: {
      channel: z.string().describe("Channel name to read status changes from"),
      after_id: z.string().optional().describe("Cursor from a previous get_status_changes call; returns only changes after this ID"),
      limit: z.number().int().min(1).max(100).optional().describe("Maximum number of changes to return (default 50)"),
    },
  },
  async (input) => {
    try {
      const params = new URLSearchParams();
      if (input.after_id) params.set("after_id", input.after_id);
      if (input.limit !== undefined) params.set("limit", String(input.limit));
      const qs = params.size > 0 ? `?${params}` : "";
      const result = await api(
        "GET",
        `/api/channels/${encodeURIComponent(input.channel)}/status/changes${qs}`
      );
      return ok(result);
    } catch (e) {
      return err(e);
    }
  }
);

server.registerTool(
  "delete_status",
  {
    description:
      "Delete a status key from a channel. Use this to clean up status entries that are no longer relevant.",
    inputSchema: {
      channel: z.string().describe("Channel name"),
      key: z.string().describe("Status key to delete"),
    },
  },
  async (input) => {
    try {
      const url = `${API_URL}/api/channels/${encodeURIComponent(input.channel)}/status/${encodeURIComponent(input.key)}`;
      const res = await fetch(url, {
        method: "DELETE",
        headers: { "Authorization": `Bearer ${API_KEY}` },
      });
      if (res.status === 204) return ok({ deleted: true, key: input.key });
      if (res.status === 404) return ok({ deleted: false, error: "Status key not found" });
      const text = await res.text();
      throw new Error(`API error ${res.status}: ${text}`);
    } catch (e) {
      return err(e);
    }
  }
);

server.registerTool(
  "delete_channel",
  {
    description:
      "Delete a channel and all its data (messages, acks, status, status log). This is irreversible.",
    inputSchema: {
      channel: z.string().describe("Channel name to delete"),
    },
  },
  async (input) => {
    try {
      const result = await api("DELETE", `/api/channels/${encodeURIComponent(input.channel)}`);
      return ok(result);
    } catch (e) {
      return err(e);
    }
  }
);

server.registerTool(
  "delete_message",
  {
    description:
      "Delete a specific message by its ID from a channel. This is irreversible.",
    inputSchema: {
      channel: z.string().describe("Channel name"),
      message_id: z.string().describe("The message ID to delete (from a read_messages response)"),
    },
  },
  async (input) => {
    try {
      const result = await api(
        "DELETE",
        `/api/channels/${encodeURIComponent(input.channel)}/messages/${encodeURIComponent(input.message_id)}`
      );
      return ok(result);
    } catch (e) {
      return err(e);
    }
  }
);

const transport = new StdioServerTransport();
await server.connect(transport);
