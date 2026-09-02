import type { ConfigView, ConversationHistory, HistoryMessage, HistoryTurn, SessionInfo, TraceRun } from "./types";

async function requestJSON<T>(url: string, init?: RequestInit): Promise<T> {
  const response = await fetch(url, init);
  if (!response.ok) {
    const detail = await response.text().catch(() => "");
    throw new Error(detail || `HTTP ${response.status}`);
  }

  const contentType = response.headers.get("content-type")?.toLowerCase() ?? "";
  if (!contentType.includes("application/json")) {
    throw new Error(
      `接口 ${url} 返回了非 JSON 内容（${contentType || "未知 Content-Type"}）。` +
      "如果正在使用 npm run dev，请确认 yomi 后端已启动，并检查 VITE_YOMI_API_TARGET。",
    );
  }
  return response.json() as Promise<T>;
}

export async function fetchSessions(): Promise<SessionInfo[]> {
  const data = await requestJSON<{ sessions: SessionInfo[] }>("/api/sessions");
  return data.sessions ?? [];
}

export async function fetchHistory(session: string): Promise<ConversationHistory> {
  const query = encodeURIComponent(session);
  const data = await requestJSON<{ messages?: HistoryMessage[]; turns?: HistoryTurn[] }>(`/api/session/messages?session=${query}`);
  return { messages: data.messages ?? [], turns: data.turns ?? [] };
}

export async function sendMessage(session: string, text: string): Promise<void> {
  await requestJSON("/api/send", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ session, text }),
  });
}

export async function cancelRun(session: string): Promise<void> {
  await requestJSON("/api/cancel", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ session }),
  });
}

export function fetchConfig(): Promise<ConfigView> {
  return requestJSON<ConfigView>("/api/config");
}

export async function saveConfig(values: Record<string, string>): Promise<void> {
  await requestJSON("/api/config", {
    method: "PUT",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(values),
  });
}

export async function fetchRuns(session: string, status = ""): Promise<TraceRun[]> {
  const params = new URLSearchParams({ session });
  if (status) params.set("status", status);
  const data = await requestJSON<{ runs: TraceRun[] }>(`/api/runs?${params}`);
  return data.runs ?? [];
}

export function fetchRun(runID: string): Promise<TraceRun> {
  return requestJSON<TraceRun>(`/api/traces/run?run_id=${encodeURIComponent(runID)}`);
}
