import express, { type Request, type Response, type NextFunction } from "express";
import cors from "cors";
import path from "path";
import { fileURLToPath } from "url";
import { Redis } from "ioredis";
import { createTeam, createApiKey, validateApiKey } from "./auth.js";
import { RelayStore, isValidCursor } from "./redis.js";
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

const redis = new Redis(REDIS_URL, { lazyConnect: true, maxRetriesPerRequest: 3 });
const app = express();

app.use(cors());
app.use(express.json());
app.use(express.static(path.resolve(__dirname, "../public")));

// ---- Structured error helper ----
function apiError(res: Response, status: number, code: string, message: string): void {
  res.status(status).json({ error: message, code });
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
    version: "0.3.0",
    endpoints: [
      { method: "POST", path: "/api/signup", description: "Create a team and get an API key", auth: false },
      { method: "POST", path: "/api/keys", description: "Create an additional API key for the team" },
      { method: "GET", path: "/api/channels", description: "List all channels" },
      { method: "POST", path: "/api/channels", description: "Create a channel" },
      { method: "DELETE", path: "/api/channels/:channel", description: "Delete a channel and all its data" },
      { method: "GET", path: "/api/channels/:channel/messages", description: "Read messages (query: after_id, limit, mention, sender)" },
      { method: "POST", path: "/api/channels/:channel/messages", description: "Post a message" },
      { method: "DELETE", path: "/api/channels/:channel/messages/:id", description: "Delete a message" },
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
  });
});

// ---- Auth middleware — applied to /api/* except POST /api/signup and GET /api ----
const authMiddleware = async (req: Request, res: Response, next: NextFunction): Promise<void> => {
  const authHeader = req.headers.authorization;
  const xApiKey = req.headers["x-api-key"];

  let key: string | undefined;
  if (authHeader?.startsWith("Bearer ")) {
    key = authHeader.slice(7);
  } else if (typeof xApiKey === "string") {
    key = xApiKey;
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
    const { content, mention } = req.body as { content?: string; mention?: string };
    if (!content) {
      apiError(res, 400, "MISSING_FIELDS", "content is required");
      return;
    }
    const store = new RelayStore(redis, req.apiKey!.team_id);
    const ch = req.params.channel as string;
    const message = await store.postMessage(ch, req.apiKey!.sender, content, mention);
    res.status(201).json(message);
  } catch (err) {
    console.error("POST /api/channels/:channel/messages error:", err);
    apiError(res, 500, "INTERNAL_ERROR", "Internal server error");
  }
});

// GET /api/channels/:channel/messages
app.get("/api/channels/:channel/messages", async (req: Request, res: Response): Promise<void> => {
  try {
    const { after_id, limit, mention, sender } = req.query as {
      after_id?: string;
      limit?: string;
      mention?: string;
      sender?: string;
    };
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
    const ch = req.params.channel as string;
    const result = await store.readMessages(ch, after_id, parsedLimit, mention, sender);
    res.json(result);
  } catch (err) {
    console.error("GET /api/channels/:channel/messages error:", err);
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
