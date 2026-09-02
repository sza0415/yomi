export type ViewName = "chat" | "trace" | "config";

export interface ToolCallRecord {
  id: string;
  name: string;
  arguments: unknown;
}

export interface HistoryMessage {
  role: "user" | "assistant" | "tool" | "system";
  content?: string;
  reasoning?: string;
  toolCalls?: ToolCallRecord[];
  toolCallId?: string;
}

export interface HistoryTurn {
  runId: string;
  user: string;
  assistant: string;
  status?: string;
  activities: AgentActivity[];
}

export interface ConversationHistory {
  messages: HistoryMessage[];
  turns: HistoryTurn[];
}

export type ActivityKind = "reasoning" | "tool" | "approval";
export type ActivityStatus = "running" | "waiting" | "completed" | "failed";

export interface AgentActivity {
  id: string;
  kind: ActivityKind;
  title: string;
  content: string;
  result?: string;
  status: ActivityStatus;
  runId?: string;
  options?: string[];
  answer?: string;
}

export interface ChatMessage {
  id: string;
  runId?: string;
  role: "user" | "assistant" | "question" | "notice";
  content: string;
  activities: AgentActivity[];
  streaming?: boolean;
  options?: string[];
  optionsDisabled?: boolean;
}

export interface SessionInfo {
  id: string;
  title: string;
  preview: string;
  message_count: number;
  created_at: string;
  updated_at: string;
}

export interface ConfigItem {
  key: string;
  env?: string;
  value?: string;
  default?: string;
  description?: string;
  status?: string;
  sensitive?: boolean;
  restart_required?: boolean;
}

export interface ConfigSection {
  id: string;
  title: string;
  description?: string;
  items: ConfigItem[];
}

export interface ConfigView {
  sections: ConfigSection[];
  editable?: boolean;
  file?: string;
  values?: Record<string, string>;
}

export interface TraceEvent {
  type: string;
  status?: string;
  timestamp?: string;
  duration_ms?: number;
  sequence?: number;
  data?: Record<string, unknown>;
}

export interface TraceRun {
  run_id: string;
  session_id?: string;
  status?: string;
  status_reason?: string;
  input_text?: string;
  model_status?: string;
  model_calls?: number;
  tool_calls?: number;
  event_count?: number;
  trace_complete?: boolean;
  trace_warning?: string;
  tool_statuses?: Record<string, string>;
  events?: TraceEvent[];
}

export interface AguiEvent {
  type: string;
  sessionId?: string;
  runId?: string;
  messageId?: string;
  role?: string;
  delta?: string;
  toolCallId?: string;
  toolCallName?: string;
  content?: string;
  name?: string;
  value?: {
    question?: string;
    options?: string[];
  };
}
