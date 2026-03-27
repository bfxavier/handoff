import { Redis } from "ioredis";
import crypto from "crypto";
import type { Team, ApiKey } from "./types.js";

const TEAMS_KEY = "auth:teams";
const teamKey = (id: string) => `auth:team:${id}`;
const API_KEYS_KEY = "auth:keys";

function generateId(): string {
  return crypto.randomBytes(12).toString("hex");
}

function generateApiKey(): string {
  return `relay_${crypto.randomBytes(24).toString("hex")}`;
}

export async function createTeam(
  redis: Redis,
  name: string,
  senderName: string
): Promise<{ team: Team; apiKey: string }> {
  const id = generateId();
  const now = new Date().toISOString();
  const team: Team = { id, name, created_at: now };

  await redis.hset(teamKey(id), {
    id,
    name,
    created_at: now,
  });
  await redis.sadd(TEAMS_KEY, id);

  const apiKey = await createApiKey(redis, id, senderName);

  return { team, apiKey };
}

export async function createApiKey(
  redis: Redis,
  teamId: string,
  senderName: string
): Promise<string> {
  const key = generateApiKey();
  const now = new Date().toISOString();
  const data: ApiKey = {
    key,
    team_id: teamId,
    sender: senderName,
    created_at: now,
  };

  await redis.hset(API_KEYS_KEY, key, JSON.stringify(data));

  return key;
}

export async function validateApiKey(
  redis: Redis,
  key: string
): Promise<ApiKey | null> {
  const raw = await redis.hget(API_KEYS_KEY, key);
  if (!raw) return null;
  return JSON.parse(raw) as ApiKey;
}

export async function getTeam(
  redis: Redis,
  teamId: string
): Promise<Team | null> {
  const raw: Record<string, string> = await redis.hgetall(teamKey(teamId));
  if (!raw.id) return null;
  return {
    id: raw.id,
    name: raw.name,
    created_at: raw.created_at,
  };
}
