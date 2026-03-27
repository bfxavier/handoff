// ---- Auth ----

export interface Team {
  id: string;
  name: string;
  created_at: string;
}

export interface ApiKey {
  key: string;
  team_id: string;
  sender: string;
  created_at: string;
}

// ---- Domain ----

export interface Channel {
  name: string;
  description: string | null;
  created_at: string;
}

export interface Message {
  id: string;
  channel: string;
  sender: string;
  content: string;
  mention: string | null;
  created_at: string;
}

export interface ReadResult {
  messages: Message[];
  next_after_id: string;
  channel: string;
}

export interface Status {
  channel: string;
  key: string;
  value: string;
  updated_by: string | null;
  updated_at: string;
}

export interface StatusChange {
  id: string;
  channel: string;
  key: string;
  value: string;
  changed_by: string | null;
  changed_at: string;
}

export interface Ack {
  channel: string;
  sender: string;
  last_read_id: string;
  acked_at: string;
}

// ---- API request/response ----

export interface SignupRequest {
  team_name: string;
  sender_name: string;
}

export interface SignupResponse {
  team: Team;
  api_key: string;
}

export interface CreateKeyRequest {
  sender_name: string;
}

export interface CreateKeyResponse {
  api_key: string;
  sender: string;
}

export interface CreateChannelRequest {
  name: string;
  description?: string;
}

export interface PostMessageRequest {
  content: string;
  mention?: string;
}

export interface ReadMessagesQuery {
  after_id?: string;
  limit?: number;
  mention?: string;
}

export interface AckRequest {
  last_read_id: string;
}

export interface SetStatusRequest {
  key: string;
  value: string;
}

export interface StatusChangesQuery {
  after_id?: string;
  limit?: number;
}
