import express, { type Request, type Response, type NextFunction } from "express";
import cors from "cors";
import path from "path";
import { fileURLToPath } from "url";
import { Redis } from "ioredis";
import { createTeam, createApiKey, validateApiKey } from "./auth.js";
import {
  RelayStore, isValidCursor, isValidChannelName,
  MAX_CONTENT_LENGTH, MAX_STATUS_KEY_LENGTH, MAX_STATUS_VALUE_LENGTH,
} from "./redis.js";
import type { ApiKey } from "./types.js";

declare global {
  namespace Express {
    interface Request {
      apiKey?: ApiKey;
    }
  }
}

const __dirname = path.dirname(fileURLToPath(import.meta.url));
const PORT = parseInt(process.env.PORT ?? "3000", 10);
const REDIS_URL = process.env.REDIS_URL ?? "redis://localhost:6379";
const RATE_LIMIT_MAX = parseInt(process.env.RATE_LIMIT_MAX ?? "100", 10);
const RATE_LIMIT_WINDOW_MS = parseInt(process.env.RATE_LIMIT_WINDOW_MS ?? "1000", 10);

const redis = new Redis(REDIS_URL, { lazyConnect: true, maxRetriesPerRequest: 3 });
const app = express();

app.use(cors());
app.use(express.json({ limit: "64kb" }));
app.use(express.static(path.resolve(__dirname, "../public")));

// ---- Structured error helper ----
function apiError(res: Response, status: number, code: string, message: string): void {
  res.status(status).json({ error: message, code });
}

// ---- JSON body parse error handler ----
app.use((err: Error & { type?: string }, _req: Request, res: Response, next: NextFunction): void => {
  if (err.type === "entity.parse.failed" || err.message?.includes("JSON")) {
    apiError(res, 400, "INVALID_JSON", "Request body must be valid JSON");
    return;
  }
  next(err);
});

// ---- Channel name validation helper ----
function validateChannelName(res: Response, name: string): boolean {
  if (!isValidChannelName(name)) {
    apiError(res, 400, "INVALID_CHANNEL_NAME",
      "Channel name must be 1-128 characters, alphanumeric with hyphens, underscores, and dots. Must start with alphanumeric.");
    return false;
  }
  return true;
}

// ---- Health checks (no auth) ----
app.get("/healthz", (_req: Request, res: Response) => {
  res.json({ status: "ok" });
});

app.get("/readyz", async (_req: Request, res: Response) => {
  try {
    await redis.ping();
    res.json({ status: "ok", redis: "connected" });
  } catch {
    res.status(503).json({ status: "unavailable", redis: "disconnected" });
  }
});

// ---- API route listing (no auth) ----
app.get("/api", (_req: Request, res: Response) => {
  res.json({
    name: "agent-relay",
    version: "0.5.0",
    endpoints: [
      { method: "POST", path: "/api/signup", description: "Create a team and get an API key", auth: false },
      { method: "POST", path: "/api/keys", description: "Create an additional API key for the team" },
      { method: "GET", path: "/api/channels", description: "List all channels" },
      { method: "POST", path: "/api/channels", description: "Create a channel (name: alphanumeric, hyphens, underscores, dots)" },
      { method: "DELETE", path: "/api/channels/:channel", description: "Delete a channel and all its data" },
      { method: "GET", path: "/api/channels/:channel/messages", description: "Read messages (query: after_id, limit, mention, sender, thread_id)" },
      { method: "POST", path: "/api/channels/:channel/messages", description: "Post a message (body: content, mention?, thread_id?)" },
      { method: "DELETE", path: "/api/channels/:channel/messages/:id", description: "Delete a message" },
      { method: "GET", path: "/api/channels/:channel/threads/:id", description: "Read a thread (parent + replies, query: after_id, limit)" },
      { method: "GET", path: "/api/channels/:channel/stream", description: "SSE stream of new messages (query: token, last_event_id)" },
      { method: "POST", path: "/api/channels/:channel/ack", description: "Acknowledge messages" },
      { method: "GET", path: "/api/channels/:channel/acks", description: "Get ack state for all agents" },
      { method: "GET", path: "/api/channels/:channel/status", description: "Get status entries (query: key)" },
      { method: "PUT", path: "/api/channels/:channel/status", description: "Set a status entry" },
      { method: "POST", path: "/api/channels/:channel/status", description: "Set a status entry (alias for PUT)" },
      { method: "GET", path: "/api/channels/:channel/status/changes", description: "Read status change log (query: after_id, limit)" },
      { method: "GET", path: "/api/status", description: "Cross-channel status query (query: channel, key)" },
      { method: "GET", path: "/healthz", description: "Liveness probe", auth: false },
      { method: "GET", path: "/readyz", description: "Readiness probe (checks Redis)", auth: false },
    ],
    limits: {
      message_content: `${MAX_CONTENT_LENGTH} bytes`,
      status_key: `${MAX_STATUS_KEY_LENGTH} chars`,
      status_value: `${MAX_STATUS_VALUE_LENGTH} bytes`,
      channel_name: "1-128 chars, alphanumeric/hyphens/underscores/dots",
      rate_limit: `${RATE_LIMIT_MAX} requests per ${RATE_LIMIT_WINDOW_MS}ms window`,
    },
  });
});

// ---- Auth middleware ----
const authMiddleware = async (req: Request, res: Response, next: NextFunction): Promise<void> => {
  const authHeader = req.headers.authorization;
  const xApiKey = req.headers["x-api-key"];
  const tokenParam = req.query.token as string | undefined;

  let key: string | undefined;
  if (authHeader?.startsWith("Bearer ")) {
    key = authHeader.slice(7);
  } else if (typeof xApiKey === "string") {
    key = xApiKey;
  } else if (tokenParam) {
    key = tokenParam;
  }

  if (!key) {
    apiError(res, 401, "MISSING_API_KEY", "Missing API key");
    return;
  }

  const apiKey = await validateApiKey(redis, key);
  if (!apiKey) {
    apiError(res, 401, "INVALID_API_KEY", "Invalid API key");
    return;
  }

  req.apiKey = apiKey;
  next();
};

app.use("/api", (req: Request, res: Response, next: NextFunction) => {
  if (req.method === "POST" && req.path === "/signup") return next();
  if (req.method === "GET" && req.path === "/") return next();
  return authMiddleware(req, res, next);
});

// ---- Rate limiting middleware (after auth, so we have the API key) ----
const rateLimitMiddleware = async (req: Request, res: Response, next: NextFunction): Promise<void> => {
  if (!req.apiKey) return next();
  const store = new RelayStore(redis, req.apiKey.team_id);
  const { allowed, remaining } = await store.checkRateLimit(req.apiKey.key, RATE_LIMIT_MAX, RATE_LIMIT_WINDOW_MS);
  res.setHeader("X-RateLimit-Limit", RATE_LIMIT_MAX);
  res.setHeader("X-RateLimit-Remaining", remaining);
  if (!allowed) {
    apiError(res, 429, "RATE_LIMITED", "Too many requests. Slow down.");
    return;
  }
  next();
};

app.use("/api", (req: Request, res: Response, next: NextFunction) => {
  if (req.method === "POST" && req.path === "/signup") return next();
  if (req.method === "GET" && req.path === "/") return next();
  return rateLimitMiddleware(req, res, next);
});

// POST /api/signup
app.post("/api/signup", async (req: Request, res: Response): Promise<void> => {
  try {
    const { team_name, sender_name } = req.body as { team_name?: string; sender_name?: string };
    if (!team_name || !sender_name) {
      apiError(res, 400, "MISSING_FIELDS", "team_name and sender_name are required");
      return;
    }
    const { team, apiKey } = await createTeam(redis, team_name, sender_name);
    res.status(201).json({ team, api_key: apiKey });
  } catch (err) {
    console.error("POST /api/signup error:", err);
    apiError(res, 500, "INTERNAL_ERROR", "Internal server error");
  }
});

// POST /api/keys
app.post("/api/keys", async (req: Request, res: Response): Promise<void> => {
  try {
    const { sender_name } = req.body as { sender_name?: string };
    if (!sender_name) {
      apiError(res, 400, "MISSING_FIELDS", "sender_name is required");
      return;
    }
    const apiKey = await createApiKey(redis, req.apiKey!.team_id, sender_name);
    res.status(201).json({ api_key: apiKey, sender: sender_name });
  } catch (err) {
    console.error("POST /api/keys error:", err);
    apiError(res, 500, "INTERNAL_ERROR", "Internal server error");
  }
});

// POST /api/channels
app.post("/api/channels", async (req: Request, res: Response): Promise<void> => {
  try {
    const { name, description } = req.body as { name?: string; description?: string };
    if (!name) {
      apiError(res, 400, "MISSING_FIELDS", "name is required");
      return;
    }
    if (!validateChannelName(res, name)) return;
    const store = new RelayStore(redis, req.apiKey!.team_id);
    const { channel, created } = await store.createChannel(name, description);
    res.status(created ? 201 : 200).json(channel);
  } catch (err) {
    console.error("POST /api/channels error:", err);
    apiError(res, 500, "INTERNAL_ERROR", "Internal server error");
  }
});

// GET /api/channels
app.get("/api/channels", async (req: Request, res: Response): Promise<void> => {
  try {
    const store = new RelayStore(redis, req.apiKey!.team_id);
    const channels = await store.listChannels();
    res.json(channels);
  } catch (err) {
    console.error("GET /api/channels error:", err);
    apiError(res, 500, "INTERNAL_ERROR", "Internal server error");
  }
});

// DELETE /api/channels/:channel
app.delete("/api/channels/:channel", async (req: Request, res: Response): Promise<void> => {
  try {
    const store = new RelayStore(redis, req.apiKey!.team_id);
    const deleted = await store.deleteChannel(req.params.channel as string);
    if (!deleted) {
      apiError(res, 404, "CHANNEL_NOT_FOUND", "Channel not found");
      return;
    }
    res.status(204).end();
  } catch (err) {
    console.error("DELETE /api/channels/:channel error:", err);
    apiError(res, 500, "INTERNAL_ERROR", "Internal server error");
  }
});

// POST /api/channels/:channel/messages
app.post("/api/channels/:channel/messages", async (req: Request, res: Response): Promise<void> => {
  try {
    const { content, mention, thread_id } = req.body as { content?: string; mention?: string; thread_id?: string };
    if (!content) {
      apiError(res, 400, "MISSING_FIELDS", "content is required");
      return;
    }
    if (content.length > MAX_CONTENT_LENGTH) {
      apiError(res, 400, "CONTENT_TOO_LARGE", `content must be ${MAX_CONTENT_LENGTH} bytes or less`);
      return;
    }
    if (thread_id && !isValidCursor(thread_id)) {
      apiError(res, 400, "INVALID_CURSOR", "Invalid thread_id format. Expected a message ID like '1234567890-0'");
      return;
    }
    const store = new RelayStore(redis, req.apiKey!.team_id);
    const ch = req.params.channel as string;
    // Validate thread parent exists
    if (thread_id) {
      const exists = await store.messageExists(ch, thread_id);
      if (!exists) {
        apiError(res, 404, "THREAD_PARENT_NOT_FOUND", "The message you are replying to does not exist");
        return;
      }
    }
    const message = await store.postMessage(ch, req.apiKey!.sender, content, mention, thread_id);
    res.status(201).json(message);
  } catch (err) {
    console.error("POST /api/channels/:channel/messages error:", err);
    apiError(res, 500, "INTERNAL_ERROR", "Internal server error");
  }
});

// GET /api/channels/:channel/messages
app.get("/api/channels/:channel/messages", async (req: Request, res: Response): Promise<void> => {
  try {
    const { after_id, limit, mention, sender, thread_id } = req.query as {
      after_id?: string;
      limit?: string;
      mention?: string;
      sender?: string;
      thread_id?: string;
    };
    if (after_id && !isValidCursor(after_id)) {
      apiError(res, 400, "INVALID_CURSOR", "Invalid after_id format. Expected a stream ID like '1234567890-0'");
      return;
    }
    if (thread_id && !isValidCursor(thread_id)) {
      apiError(res, 400, "INVALID_CURSOR", "Invalid thread_id format");
      return;
    }
    const parsedLimit = limit !== undefined ? parseInt(limit, 10) : undefined;
    if (parsedLimit !== undefined && (isNaN(parsedLimit) || parsedLimit < 1 || parsedLimit > 100)) {
      apiError(res, 400, "INVALID_LIMIT", "limit must be an integer between 1 and 100");
      return;
    }
    const store = new RelayStore(redis, req.apiKey!.team_id);
    const ch = req.params.channel as string;
    const result = await store.readMessages(ch, after_id, parsedLimit, mention, sender, thread_id);
    res.json(result);
  } catch (err) {
    console.error("GET /api/channels/:channel/messages error:", err);
    apiError(res, 500, "INTERNAL_ERROR", "Internal server error");
  }
});

// GET /api/channels/:channel/threads/:id
app.get("/api/channels/:channel/threads/:id", async (req: Request, res: Response): Promise<void> => {
  try {
    const parentId = req.params.id as string;
    if (!isValidCursor(parentId)) {
      apiError(res, 400, "INVALID_CURSOR", "Invalid thread ID format");
      return;
    }
    const { after_id, limit } = req.query as { after_id?: string; limit?: string };
    if (after_id && !isValidCursor(after_id)) {
      apiError(res, 400, "INVALID_CURSOR", "Invalid after_id format");
      return;
    }
    const parsedLimit = limit !== undefined ? parseInt(limit, 10) : undefined;
    if (parsedLimit !== undefined && (isNaN(parsedLimit) || parsedLimit < 1 || parsedLimit > 100)) {
      apiError(res, 400, "INVALID_LIMIT", "limit must be an integer between 1 and 100");
      return;
    }
    const store = new RelayStore(redis, req.apiKey!.team_id);
    const result = await store.readThread(req.params.channel as string, parentId, after_id, parsedLimit);
    res.json(result);
  } catch (err) {
    if (err instanceof Error && err.message === "PARENT_NOT_FOUND") {
      apiError(res, 404, "MESSAGE_NOT_FOUND", "Parent message not found");
      return;
    }
    console.error("GET /api/channels/:channel/threads/:id error:", err);
    apiError(res, 500, "INTERNAL_ERROR", "Internal server error");
  }
});

// DELETE /api/channels/:channel/messages/:id
app.delete("/api/channels/:channel/messages/:id", async (req: Request, res: Response): Promise<void> => {
  try {
    const messageId = req.params.id as string;
    if (!isValidCursor(messageId)) {
      apiError(res, 400, "INVALID_CURSOR", "Invalid message ID format");
      return;
    }
    const store = new RelayStore(redis, req.apiKey!.team_id);
    const deleted = await store.deleteMessage(req.params.channel as string, messageId);
    if (!deleted) {
      apiError(res, 404, "MESSAGE_NOT_FOUND", "Message not found");
      return;
    }
    res.status(204).end();
  } catch (err) {
    console.error("DELETE /api/channels/:channel/messages/:id error:", err);
    apiError(res, 500, "INTERNAL_ERROR", "Internal server error");
  }
});

// ---- SSE: GET /api/channels/:channel/stream ----
// Auth already handled by middleware (supports ?token= query param)
app.get("/api/channels/:channel/stream", async (req: Request, res: Response): Promise<void> => {
  const apiKey = req.apiKey!;
  const channel = req.params.channel as string;
  const lastEventId = (req.headers["last-event-id"] as string) || (req.query.last_event_id as string) || "$";

  // Validate last_event_id if provided
  if (lastEventId !== "$" && !isValidCursor(lastEventId)) {
    apiError(res, 400, "INVALID_CURSOR", "Invalid last_event_id format");
    return;
  }

  // Disable Nagle's algorithm so writes flush immediately
  req.socket.setNoDelay(true);

  // Set SSE headers
  res.set({
    "Content-Type": "text/event-stream",
    "Cache-Control": "no-cache, no-transform",
    "Connection": "keep-alive",
    "X-Accel-Buffering": "no",
  });
  res.flushHeaders();
  res.write(":ok\n\n");

  // Dedicated Redis connection for blocking reads
  const subscriber = new Redis(REDIS_URL, { lazyConnect: true, maxRetriesPerRequest: null });
  await subscriber.connect();

  // Helper: write SSE data and force flush through any buffering
  const sseWrite = (data: string) => {
    res.write(data);
    // Force flush through Node's internal buffer
    if (typeof (res as any).flush === "function") {
      (res as any).flush();
    }
  };

  const store = new RelayStore(redis, apiKey.team_id);
  let cursor = lastEventId === "$" ? "$" : lastEventId;
  let closed = false;

  // If reconnecting with a cursor, first send any messages we missed
  if (cursor !== "$") {
    try {
      const catchUp = await store.readMessages(channel, cursor, 100);
      for (const msg of catchUp.messages) {
        if (closed) break;
        sseWrite(`id: ${msg.id}\nevent: message\ndata: ${JSON.stringify(msg)}\n\n`);
        cursor = msg.id;
      }
    } catch {
      // Channel may not exist yet, that's fine
    }
  }

  // Get the latest ID if starting fresh
  if (cursor === "$") {
    const streamKey = `t:${apiKey.team_id}:msg:${channel}`;
    const info = await redis.xinfo("STREAM", streamKey).catch(() => null);
    if (info) {
      const infoArr = info as unknown[];
      for (let i = 0; i < infoArr.length; i += 2) {
        if (infoArr[i] === "last-generated-id") {
          cursor = infoArr[i + 1] as string;
          break;
        }
      }
    }
    if (cursor === "$") cursor = "0-0";
  }

  const poll = async () => {
    while (!closed) {
      try {
        const messages = await store.blockingRead(channel, cursor, 25000, subscriber);
        for (const msg of messages) {
          if (closed) break;
          sseWrite(`id: ${msg.id}\nevent: message\ndata: ${JSON.stringify(msg)}\n\n`);
          cursor = msg.id;
        }
        if (!closed && messages.length === 0) {
          sseWrite(":keepalive\n\n");
        }
      } catch (err) {
        if (!closed) {
          console.error("SSE poll error:", err);
          sseWrite(`event: error\ndata: ${JSON.stringify({ error: "Stream error" })}\n\n`);
        }
        break;
      }
    }
  };

  req.on("close", () => {
    closed = true;
    subscriber.disconnect();
  });

  poll();
});

// POST /api/channels/:channel/ack
app.post("/api/channels/:channel/ack", async (req: Request, res: Response): Promise<void> => {
  try {
    const { last_read_id } = req.body as { last_read_id?: string };
    if (!last_read_id) {
      apiError(res, 400, "MISSING_FIELDS", "last_read_id is required");
      return;
    }
    if (!isValidCursor(last_read_id)) {
      apiError(res, 400, "INVALID_CURSOR", "Invalid last_read_id format. Expected a stream ID like '1234567890-0'");
      return;
    }
    const store = new RelayStore(redis, req.apiKey!.team_id);
    const ack = await store.ackMessages(req.params.channel as string, req.apiKey!.sender, last_read_id);
    res.json(ack);
  } catch (err) {
    console.error("POST /api/channels/:channel/ack error:", err);
    apiError(res, 500, "INTERNAL_ERROR", "Internal server error");
  }
});

// GET /api/channels/:channel/acks
app.get("/api/channels/:channel/acks", async (req: Request, res: Response): Promise<void> => {
  try {
    const store = new RelayStore(redis, req.apiKey!.team_id);
    const acks = await store.getAcks(req.params.channel as string);
    res.json(acks);
  } catch (err) {
    console.error("GET /api/channels/:channel/acks error:", err);
    apiError(res, 500, "INTERNAL_ERROR", "Internal server error");
  }
});

// PUT + POST /api/channels/:channel/status
const setStatusHandler = async (req: Request, res: Response): Promise<void> => {
  try {
    const { key, value } = req.body as { key?: string; value?: string };
    if (!key || value === undefined) {
      apiError(res, 400, "MISSING_FIELDS", "key and value are required");
      return;
    }
    if (key.length > MAX_STATUS_KEY_LENGTH) {
      apiError(res, 400, "KEY_TOO_LARGE", `Status key must be ${MAX_STATUS_KEY_LENGTH} characters or less`);
      return;
    }
    if (typeof value === "string" && value.length > MAX_STATUS_VALUE_LENGTH) {
      apiError(res, 400, "VALUE_TOO_LARGE", `Status value must be ${MAX_STATUS_VALUE_LENGTH} bytes or less`);
      return;
    }
    const store = new RelayStore(redis, req.apiKey!.team_id);
    const status = await store.setStatus(req.params.channel as string, key, value, req.apiKey!.sender);
    res.json(status);
  } catch (err) {
    console.error("PUT /api/channels/:channel/status error:", err);
    apiError(res, 500, "INTERNAL_ERROR", "Internal server error");
  }
};
app.put("/api/channels/:channel/status", setStatusHandler);
app.post("/api/channels/:channel/status", setStatusHandler);

// GET /api/channels/:channel/status
app.get("/api/channels/:channel/status", async (req: Request, res: Response): Promise<void> => {
  try {
    const { key } = req.query as { key?: string };
    const store = new RelayStore(redis, req.apiKey!.team_id);
    const statuses = await store.getStatus(req.params.channel as string, key);
    res.json(statuses);
  } catch (err) {
    console.error("GET /api/channels/:channel/status error:", err);
    apiError(res, 500, "INTERNAL_ERROR", "Internal server error");
  }
});

// GET /api/status  (cross-channel)
app.get("/api/status", async (req: Request, res: Response): Promise<void> => {
  try {
    const { channel, key } = req.query as { channel?: string; key?: string };
    const store = new RelayStore(redis, req.apiKey!.team_id);
    const statuses = await store.getStatus(channel, key);
    res.json(statuses);
  } catch (err) {
    console.error("GET /api/status error:", err);
    apiError(res, 500, "INTERNAL_ERROR", "Internal server error");
  }
});

// GET /api/channels/:channel/status/changes
app.get("/api/channels/:channel/status/changes", async (req: Request, res: Response): Promise<void> => {
  try {
    const { after_id, limit } = req.query as { after_id?: string; limit?: string };
    if (after_id && !isValidCursor(after_id)) {
      apiError(res, 400, "INVALID_CURSOR", "Invalid after_id format. Expected a stream ID like '1234567890-0'");
      return;
    }
    const parsedLimit = limit !== undefined ? parseInt(limit, 10) : undefined;
    if (parsedLimit !== undefined && (isNaN(parsedLimit) || parsedLimit < 1 || parsedLimit > 100)) {
      apiError(res, 400, "INVALID_LIMIT", "limit must be an integer between 1 and 100");
      return;
    }
    const store = new RelayStore(redis, req.apiKey!.team_id);
    const result = await store.getStatusChanges(req.params.channel as string, after_id, parsedLimit);
    res.json(result);
  } catch (err) {
    console.error("GET /api/channels/:channel/status/changes error:", err);
    apiError(res, 500, "INTERNAL_ERROR", "Internal server error");
  }
});

// ---- JSON 404 catch-all ----
app.use((_req: Request, res: Response) => {
  apiError(res, 404, "NOT_FOUND", "Route not found");
});

// ---- Global error handler — always JSON, never HTML ----
app.use((err: Error, _req: Request, res: Response, _next: NextFunction): void => {
  console.error("Unhandled error:", err);
  apiError(res, 500, "INTERNAL_ERROR", "Internal server error");
});

async function start(): Promise<void> {
  await redis.connect();
  console.log(`Redis connected: ${REDIS_URL}`);

  app.listen(PORT, () => {
    console.log(`Agent Relay listening on port ${PORT}`);
  });
}

function shutdown(): void {
  console.log("Shutting down...");
  redis.quit().finally(() => process.exit(0));
}

process.on("SIGTERM", shutdown);
process.on("SIGINT", shutdown);

start().catch((err) => {
  console.error("Failed to start server:", err);
  process.exit(1);
});
