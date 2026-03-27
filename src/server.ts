import express, { type Request, type Response, type NextFunction } from "express";
import cors from "cors";
import path from "path";
import { fileURLToPath } from "url";
import { Redis } from "ioredis";
import { createTeam, createApiKey, validateApiKey } from "./auth.js";
import { RelayStore } from "./redis.js";
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

// Auth middleware — applied to /api/* except POST /api/signup
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
    res.status(401).json({ error: "Missing API key" });
    return;
  }

  const apiKey = await validateApiKey(redis, key);
  if (!apiKey) {
    res.status(401).json({ error: "Invalid API key" });
    return;
  }

  req.apiKey = apiKey;
  next();
};

app.use("/api", (req: Request, res: Response, next: NextFunction) => {
  if (req.method === "POST" && req.path === "/signup") {
    return next();
  }
  return authMiddleware(req, res, next);
});

// POST /api/signup
app.post("/api/signup", async (req: Request, res: Response): Promise<void> => {
  try {
    const { team_name, sender_name } = req.body as { team_name?: string; sender_name?: string };
    if (!team_name || !sender_name) {
      res.status(400).json({ error: "team_name and sender_name are required" });
      return;
    }
    const { team, apiKey } = await createTeam(redis, team_name, sender_name);
    res.status(201).json({ team, api_key: apiKey });
  } catch (err) {
    console.error("POST /api/signup error:", err);
    res.status(500).json({ error: "Internal server error" });
  }
});

// POST /api/keys
app.post("/api/keys", async (req: Request, res: Response): Promise<void> => {
  try {
    const { sender_name } = req.body as { sender_name?: string };
    if (!sender_name) {
      res.status(400).json({ error: "sender_name is required" });
      return;
    }
    const apiKey = await createApiKey(redis, req.apiKey!.team_id, sender_name);
    res.status(201).json({ api_key: apiKey, sender: sender_name });
  } catch (err) {
    console.error("POST /api/keys error:", err);
    res.status(500).json({ error: "Internal server error" });
  }
});

// POST /api/channels
app.post("/api/channels", async (req: Request, res: Response): Promise<void> => {
  try {
    const { name, description } = req.body as { name?: string; description?: string };
    if (!name) {
      res.status(400).json({ error: "name is required" });
      return;
    }
    const store = new RelayStore(redis, req.apiKey!.team_id);
    const channel = await store.createChannel(name, description);
    res.status(201).json(channel);
  } catch (err) {
    console.error("POST /api/channels error:", err);
    res.status(500).json({ error: "Internal server error" });
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
    res.status(500).json({ error: "Internal server error" });
  }
});

// POST /api/channels/:channel/messages
app.post("/api/channels/:channel/messages", async (req: Request, res: Response): Promise<void> => {
  try {
    const { content, mention } = req.body as { content?: string; mention?: string };
    if (!content) {
      res.status(400).json({ error: "content is required" });
      return;
    }
    const store = new RelayStore(redis, req.apiKey!.team_id);
    const ch = (req.params.channel as string) as string;
    const message = await store.postMessage(ch, req.apiKey!.sender, content, mention);
    res.status(201).json(message);
  } catch (err) {
    console.error("POST /api/channels/:channel/messages error:", err);
    res.status(500).json({ error: "Internal server error" });
  }
});

// GET /api/channels/:channel/messages
app.get("/api/channels/:channel/messages", async (req: Request, res: Response): Promise<void> => {
  try {
    const { after_id, limit, mention } = req.query as {
      after_id?: string;
      limit?: string;
      mention?: string;
    };
    const parsedLimit = limit !== undefined ? parseInt(limit, 10) : undefined;
    const store = new RelayStore(redis, req.apiKey!.team_id);
    const ch = (req.params.channel as string) as string;
    const result = await store.readMessages(ch, after_id, parsedLimit, mention);
    res.json(result);
  } catch (err) {
    console.error("GET /api/channels/:channel/messages error:", err);
    res.status(500).json({ error: "Internal server error" });
  }
});

// POST /api/channels/:channel/ack
app.post("/api/channels/:channel/ack", async (req: Request, res: Response): Promise<void> => {
  try {
    const { last_read_id } = req.body as { last_read_id?: string };
    if (!last_read_id) {
      res.status(400).json({ error: "last_read_id is required" });
      return;
    }
    const store = new RelayStore(redis, req.apiKey!.team_id);
    const ack = await store.ackMessages((req.params.channel as string), req.apiKey!.sender, last_read_id);
    res.json(ack);
  } catch (err) {
    console.error("POST /api/channels/:channel/ack error:", err);
    res.status(500).json({ error: "Internal server error" });
  }
});

// GET /api/channels/:channel/acks
app.get("/api/channels/:channel/acks", async (req: Request, res: Response): Promise<void> => {
  try {
    const store = new RelayStore(redis, req.apiKey!.team_id);
    const acks = await store.getAcks((req.params.channel as string));
    res.json(acks);
  } catch (err) {
    console.error("GET /api/channels/:channel/acks error:", err);
    res.status(500).json({ error: "Internal server error" });
  }
});

// POST /api/channels/:channel/status
app.post("/api/channels/:channel/status", async (req: Request, res: Response): Promise<void> => {
  try {
    const { key, value } = req.body as { key?: string; value?: string };
    if (!key || value === undefined) {
      res.status(400).json({ error: "key and value are required" });
      return;
    }
    const store = new RelayStore(redis, req.apiKey!.team_id);
    const status = await store.setStatus((req.params.channel as string), key, value, req.apiKey!.sender);
    res.json(status);
  } catch (err) {
    console.error("POST /api/channels/:channel/status error:", err);
    res.status(500).json({ error: "Internal server error" });
  }
});

// GET /api/channels/:channel/status
app.get("/api/channels/:channel/status", async (req: Request, res: Response): Promise<void> => {
  try {
    const { key } = req.query as { key?: string };
    const store = new RelayStore(redis, req.apiKey!.team_id);
    const statuses = await store.getStatus((req.params.channel as string), key);
    res.json(statuses);
  } catch (err) {
    console.error("GET /api/channels/:channel/status error:", err);
    res.status(500).json({ error: "Internal server error" });
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
    res.status(500).json({ error: "Internal server error" });
  }
});

// GET /api/channels/:channel/status/changes
app.get("/api/channels/:channel/status/changes", async (req: Request, res: Response): Promise<void> => {
  try {
    const { after_id, limit } = req.query as { after_id?: string; limit?: string };
    const parsedLimit = limit !== undefined ? parseInt(limit, 10) : undefined;
    const store = new RelayStore(redis, req.apiKey!.team_id);
    const result = await store.getStatusChanges((req.params.channel as string), after_id, parsedLimit);
    res.json(result);
  } catch (err) {
    console.error("GET /api/channels/:channel/status/changes error:", err);
    res.status(500).json({ error: "Internal server error" });
  }
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
