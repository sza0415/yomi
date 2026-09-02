<script setup lang="ts">
import {
  Activity,
  ArrowUp,
  Bot,
  BrainCircuit,
  Menu,
  MessageCircleMore,
  PanelLeftOpen,
  Settings,
  Square,
} from "./icons";
import { computed, defineAsyncComponent, nextTick, onBeforeUnmount, onMounted, ref } from "vue";
import { cancelRun, fetchHistory, fetchSessions, sendMessage } from "./api";
import ChatMessage from "./components/ChatMessage.vue";
import SessionSidebar from "./components/SessionSidebar.vue";
import type { AgentActivity, AguiEvent, ChatMessage as ChatMessageType, ConversationHistory, HistoryMessage, HistoryTurn, SessionInfo, ViewName } from "./types";

const ConfigWorkspace = defineAsyncComponent(() => import("./components/ConfigWorkspace.vue"));
const TraceWorkspace = defineAsyncComponent(() => import("./components/TraceWorkspace.vue"));

const configOnly = new URLSearchParams(window.location.search).get("config") === "1";
const initialSession = new URLSearchParams(window.location.search).get("session") || localStorage.getItem("szabot_session") || newSessionID();

const activeView = ref<ViewName>(configOnly ? "config" : "chat");
const session = ref(initialSession);
const sessions = ref<SessionInfo[]>([]);
const messages = ref<ChatMessageType[]>([]);
const input = ref("");
const waiting = ref(false);
const answering = ref(false);
const activeRun = ref(false);
const online = ref(false);
const statusText = ref(configOnly ? "配置向导" : "连接中");
const sidebarOpen = ref(window.innerWidth >= 1080);
const messagesViewport = ref<HTMLElement | null>(null);
const composerInput = ref<HTMLTextAreaElement | null>(null);

let eventSource: EventSource | null = null;
let currentAssistantID = "";
let animationFrame = 0;
let pendingApprovalTarget: { messageID: string; activityID: string } | null = null;

const textMessageMap = new Map<string, string>();
const reasoningMessageMap = new Map<string, { messageID: string; activityID: string }>();
const toolCallMap = new Map<string, { messageID: string; activityID: string }>();
const pendingText = new Map<string, string>();
const pendingReasoning = new Map<string, string>();

const busy = computed(() => waiting.value || activeRun.value || answering.value);
const canSend = computed(() => Boolean(input.value.trim()) && !waiting.value);
const composerPlaceholder = computed(() => answering.value ? "输入你的回答，或选择上方选项…" : "给 Yomi 发送消息…");

function newSessionID(): string {
  return `web-${Date.now()}-${Math.random().toString(36).slice(2, 8)}`;
}

function newLocalID(prefix: string): string {
  return `${prefix}-${Date.now()}-${Math.random().toString(36).slice(2, 9)}`;
}

function updateSessionLocation(): void {
  const url = new URL(window.location.href);
  url.searchParams.set("session", session.value);
  window.history.replaceState({}, "", url);
  localStorage.setItem("szabot_session", session.value);
}

function messageByID(id: string): ChatMessageType | undefined {
  return messages.value.find((message) => message.id === id);
}

function activityByID(messageID: string, activityID: string): AgentActivity | undefined {
  return messageByID(messageID)?.activities.find((activity) => activity.id === activityID);
}

function scrollToBottom(): void {
  nextTick(() => {
    const viewport = messagesViewport.value;
    if (viewport) viewport.scrollTop = viewport.scrollHeight;
  });
}

function ensureAssistant(): ChatMessageType {
  const existing = currentAssistantID ? messageByID(currentAssistantID) : undefined;
  if (existing) return existing;
  const message: ChatMessageType = {
    id: newLocalID("assistant"),
    role: "assistant",
    content: "",
    activities: [],
    streaming: true,
  };
  messages.value.push(message);
  currentAssistantID = message.id;
  scrollToBottom();
  return message;
}

function finishCurrentAssistant(): void {
  flushPendingDeltas();
  const message = currentAssistantID ? messageByID(currentAssistantID) : undefined;
  if (message) {
    message.streaming = false;
    for (const activity of message.activities) {
      if (activity.status === "running") activity.status = "completed";
    }
  }
  currentAssistantID = "";
}

function pendingApproval(): AgentActivity | undefined {
  if (!pendingApprovalTarget) return undefined;
  return activityByID(pendingApprovalTarget.messageID, pendingApprovalTarget.activityID);
}

function scheduleDeltaFlush(): void {
  if (animationFrame) return;
  animationFrame = window.requestAnimationFrame(() => {
    animationFrame = 0;
    flushPendingDeltas();
  });
}

function flushPendingDeltas(): void {
  for (const [messageID, delta] of pendingText) {
    const message = messageByID(messageID);
    if (message) message.content += delta;
  }
  pendingText.clear();
  for (const [key, delta] of pendingReasoning) {
    const [messageID, activityID] = key.split("::");
    const activity = activityByID(messageID, activityID);
    if (activity) activity.content += delta;
  }
  pendingReasoning.clear();
  scrollToBottom();
}

function queueText(messageID: string, delta: string): void {
  pendingText.set(messageID, (pendingText.get(messageID) ?? "") + delta);
  scheduleDeltaFlush();
}

function queueReasoning(messageID: string, activityID: string, delta: string): void {
  const key = `${messageID}::${activityID}`;
  pendingReasoning.set(key, (pendingReasoning.get(key) ?? "") + delta);
  scheduleDeltaFlush();
}

function normalizeArguments(value: unknown): string {
  if (typeof value === "string") return value;
  try {
    return JSON.stringify(value ?? {}, null, 2);
  } catch {
    return String(value ?? "");
  }
}

function historyToMessages(history: HistoryMessage[], turns: HistoryTurn[] = []): ChatMessageType[] {
  if (turns.length) {
    return turns.flatMap((turn) => {
      const items: ChatMessageType[] = [];
      if (turn.user) {
        items.push({ id: `${turn.runId}-user`, role: "user", content: turn.user, activities: [] });
      }
      if (turn.assistant || turn.activities.length) {
        items.push({
          id: `${turn.runId}-assistant`,
          role: "assistant",
          content: turn.assistant ?? "",
          activities: turn.activities.map((activity) => ({ ...activity })),
          streaming: turn.activities.some((activity) => activity.status === "waiting" || activity.status === "running"),
        });
      }
      return items;
    });
  }

  const result: ChatMessageType[] = [];
  const historicalTools = new Map<string, AgentActivity>();

  for (const record of history) {
    if (record.role === "user") {
      result.push({ id: newLocalID("history-user"), role: "user", content: record.content ?? "", activities: [] });
      continue;
    }
    if (record.role === "assistant") {
      const activities: AgentActivity[] = [];
      if (record.reasoning) {
        activities.push({ id: newLocalID("reasoning"), kind: "reasoning", title: "思考过程", content: record.reasoning, status: "completed" });
      }
      for (const call of record.toolCalls ?? []) {
        const activity: AgentActivity = {
          id: newLocalID("tool"), kind: "tool", title: call.name || "工具调用",
          content: normalizeArguments(call.arguments), status: "completed",
        };
        activities.push(activity);
        if (call.id) historicalTools.set(call.id, activity);
      }
      result.push({ id: newLocalID("history-assistant"), role: "assistant", content: record.content ?? "", activities });
      continue;
    }
    if (record.role === "tool") {
      const activity = record.toolCallId ? historicalTools.get(record.toolCallId) : undefined;
      if (activity) {
        activity.result = record.content ?? "";
      } else {
        const previous = [...result].reverse().find((message) => message.role === "assistant");
        const target = previous ?? { id: newLocalID("history-assistant"), role: "assistant" as const, content: "", activities: [] };
        target.activities.push({ id: newLocalID("tool-result"), kind: "tool", title: "工具结果", content: "", result: record.content ?? "", status: "completed" });
        if (!previous) result.push(target);
      }
    }
  }
  return result;
}

function applyHistory(history: ConversationHistory): void {
  messages.value = historyToMessages(history.messages, history.turns);
  pendingApprovalTarget = null;
  answering.value = false;
  toolCallMap.clear();

  for (let index = messages.value.length - 1; index >= 0; index--) {
    const message = messages.value[index];
    for (const activity of message.activities) {
      if (activity.kind === "tool" && activity.id) {
        toolCallMap.set(activity.id, { messageID: message.id, activityID: activity.id });
      }
    }
    const activity = [...message.activities].reverse().find((item) => item.kind === "approval" && item.status === "waiting");
    if (!activity) continue;
    currentAssistantID = message.id;
    pendingApprovalTarget = { messageID: message.id, activityID: activity.id };
    answering.value = true;
    activeRun.value = true;
    waiting.value = false;
    break;
  }
}

async function loadInitialData(): Promise<void> {
  const [sessionResult, historyResult] = await Promise.allSettled([
    fetchSessions(),
    fetchHistory(session.value),
  ]);
  if (sessionResult.status === "fulfilled") sessions.value = sessionResult.value;
  if (historyResult.status === "fulfilled") applyHistory(historyResult.value);
  else messages.value = [{ id: newLocalID("notice"), role: "notice", content: `历史加载失败：${historyResult.reason}`, activities: [] }];
}

async function refreshSessions(): Promise<void> {
  try {
    sessions.value = await fetchSessions();
  } catch {
    // Session discovery is secondary; the active conversation can continue.
  }
}

function connect(): void {
  eventSource?.close();
  eventSource = new EventSource(`/api/stream?session=${encodeURIComponent(session.value)}`);
  eventSource.onmessage = (event) => {
    try {
      handleAguiEvent(JSON.parse(event.data) as AguiEvent);
    } catch {
      // Ignore malformed third-party/proxy events and keep the stream alive.
    }
  };
  eventSource.onerror = () => {
    online.value = false;
    statusText.value = "连接断开，正在重连";
  };
}

function handleAguiEvent(event: AguiEvent): void {
  switch (event.type) {
    case "SESSION":
      online.value = true;
      statusText.value = "在线";
      return;
    case "RUN_STARTED":
      activeRun.value = true;
      return;
    case "TEXT_MESSAGE_START": {
      const assistant = ensureAssistant();
      assistant.streaming = true;
      if (event.messageId) textMessageMap.set(event.messageId, assistant.id);
      return;
    }
    case "TEXT_MESSAGE_CONTENT": {
      if (!event.messageId) return;
      let messageID = textMessageMap.get(event.messageId);
      if (!messageID) {
        messageID = ensureAssistant().id;
        textMessageMap.set(event.messageId, messageID);
      }
      queueText(messageID, event.delta ?? "");
      return;
    }
    case "TEXT_MESSAGE_END":
      if (event.messageId) textMessageMap.delete(event.messageId);
      return;
    case "REASONING_MESSAGE_START": {
      const assistant = ensureAssistant();
      const activity: AgentActivity = { id: newLocalID("reasoning"), kind: "reasoning", title: "思考过程", content: "", status: "running" };
      assistant.activities.push(activity);
      if (event.messageId) reasoningMessageMap.set(event.messageId, { messageID: assistant.id, activityID: activity.id });
      return;
    }
    case "REASONING_MESSAGE_CONTENT": {
      if (!event.messageId) return;
      let target = reasoningMessageMap.get(event.messageId);
      if (!target) {
        const assistant = ensureAssistant();
        const activity: AgentActivity = { id: newLocalID("reasoning"), kind: "reasoning", title: "思考过程", content: "", status: "running" };
        assistant.activities.push(activity);
        target = { messageID: assistant.id, activityID: activity.id };
        reasoningMessageMap.set(event.messageId, target);
      }
      queueReasoning(target.messageID, target.activityID, event.delta ?? "");
      return;
    }
    case "REASONING_MESSAGE_END": {
      if (!event.messageId) return;
      const target = reasoningMessageMap.get(event.messageId);
      if (target) {
        flushPendingDeltas();
        const activity = activityByID(target.messageID, target.activityID);
        if (activity) activity.status = "completed";
      }
      reasoningMessageMap.delete(event.messageId);
      return;
    }
    case "TOOL_CALL_START": {
      const assistant = ensureAssistant();
      const activity: AgentActivity = {
        id: newLocalID("tool"), kind: "tool", title: event.toolCallName || "工具调用",
        content: "", status: "running",
      };
      assistant.activities.push(activity);
      if (event.toolCallId) toolCallMap.set(event.toolCallId, { messageID: assistant.id, activityID: activity.id });
      scrollToBottom();
      return;
    }
    case "TOOL_CALL_ARGS": {
      if (!event.toolCallId) return;
      const target = toolCallMap.get(event.toolCallId);
      if (target) {
        const activity = activityByID(target.messageID, target.activityID);
        if (activity) activity.content += event.delta ?? "";
      }
      return;
    }
    case "TOOL_CALL_END":
      return;
    case "TOOL_CALL_RESULT": {
      if (!event.toolCallId) return;
      const target = toolCallMap.get(event.toolCallId);
      if (target) {
        const activity = activityByID(target.messageID, target.activityID);
        if (activity) {
          activity.result = event.content || "（空结果）";
          activity.status = "completed";
        }
      } else {
        // SSE 重连可能错过 TOOL_CALL_START。结果仍应显示，而不是静默丢弃。
        const assistant = ensureAssistant();
        assistant.activities.push({
          id: newLocalID("tool"),
          kind: "tool",
          title: event.toolCallName || "工具调用",
          content: "",
          result: event.content || "（空结果）",
          status: "completed",
        });
      }
      return;
    }
    case "CUSTOM":
      if (event.name === "ASK_USER_QUESTION" && event.value) {
        showQuestion(event.value.question ?? "", event.value.options ?? [], event.runId);
      }
      return;
    case "RUN_FINISHED":
      finishCurrentAssistant();
      pendingApprovalTarget = null;
      waiting.value = false;
      answering.value = false;
      activeRun.value = false;
      refreshSessions();
      return;
  }
}

function showQuestion(question: string, options: string[], runId?: string): void {
  flushPendingDeltas();

  // pending 问题会在 SSE 重连时重放。复用已有步骤，避免生成两张确认卡。
  const duplicate = messages.value
    .flatMap((message) => message.activities.map((activity) => ({ message, activity })))
    .find(({ activity }) =>
      activity.kind === "approval" &&
      activity.status === "waiting" &&
      ((runId && activity.runId === runId) ||
        (!runId && activity.content === question && JSON.stringify(activity.options ?? []) === JSON.stringify(options))),
    );
  if (duplicate) {
    currentAssistantID = duplicate.message.id;
    pendingApprovalTarget = { messageID: duplicate.message.id, activityID: duplicate.activity.id };
    answering.value = true;
    waiting.value = false;
    activeRun.value = true;
    scrollToBottom();
    return;
  }

  const assistant = ensureAssistant();
  const activity: AgentActivity = {
    id: newLocalID("approval"),
    kind: "approval",
    title: options.includes("Allow once") ? "工具权限确认" : "需要你的回答",
    content: question,
    status: "waiting",
    runId,
    options,
  };
  assistant.activities.push(activity);
  assistant.streaming = true;
  pendingApprovalTarget = { messageID: assistant.id, activityID: activity.id };
  answering.value = true;
  waiting.value = false;
  activeRun.value = true;
  scrollToBottom();
  nextTick(() => composerInput.value?.focus());
}

async function submitMessage(forcedText?: string, approvalActivityID?: string): Promise<void> {
  const text = (forcedText ?? input.value).trim();
  if (!text || waiting.value) return;

  const answeringQuestion = answering.value;
  const approval = pendingApproval();
  if (answeringQuestion) {
    if (approvalActivityID && approval?.id !== approvalActivityID) return;
    if (approval) {
      approval.answer = text;
      approval.status = "completed";
    }
    answering.value = false;
  }

  input.value = "";
  resizeComposer();
  if (!answeringQuestion) {
    finishCurrentAssistant();
    messages.value.push({ id: newLocalID("user"), role: "user", content: text, activities: [] });
  }
  waiting.value = true;
  activeRun.value = true;
  scrollToBottom();

  try {
    await sendMessage(session.value, text);
    if (answeringQuestion) pendingApprovalTarget = null;
  } catch (cause) {
    if (answeringQuestion && approval) {
      approval.answer = undefined;
      approval.status = "waiting";
      pendingApprovalTarget = { messageID: currentAssistantID, activityID: approval.id };
      answering.value = true;
    }
    messages.value.push({ id: newLocalID("notice"), role: "notice", content: `发送失败：${cause instanceof Error ? cause.message : String(cause)}`, activities: [] });
    waiting.value = false;
    activeRun.value = false;
  }
}

async function cancelActiveRun(): Promise<void> {
  if (!activeRun.value) return;
  try {
    await cancelRun(session.value);
    const approval = pendingApproval();
    if (approval) approval.status = "failed";
    pendingApprovalTarget = null;
    finishCurrentAssistant();
    waiting.value = false;
    answering.value = false;
    activeRun.value = false;
    messages.value.push({ id: newLocalID("notice"), role: "notice", content: "任务已取消。", activities: [] });
  } catch (cause) {
    messages.value.push({ id: newLocalID("notice"), role: "notice", content: `取消失败：${cause instanceof Error ? cause.message : String(cause)}`, activities: [] });
  }
}

async function switchSession(nextSession: string): Promise<void> {
  if (!nextSession || nextSession === session.value || busy.value) return;
  finishCurrentAssistant();
  pendingApprovalTarget = null;
  textMessageMap.clear();
  reasoningMessageMap.clear();
  toolCallMap.clear();
  session.value = nextSession;
  updateSessionLocation();
  const history = await fetchHistory(session.value).catch(() => ({ messages: [], turns: [] }));
  applyHistory(history);
  sidebarOpen.value = window.innerWidth >= 1080;
  connect();
  scrollToBottom();
}

async function createSession(): Promise<void> {
  if (busy.value) return;
  await switchSession(newSessionID());
  messages.value = [];
}

function resizeComposer(): void {
  nextTick(() => {
    const element = composerInput.value;
    if (!element) return;
    element.style.height = "auto";
    element.style.height = `${Math.min(element.scrollHeight, 180)}px`;
  });
}

function handleComposerKeydown(event: KeyboardEvent): void {
  if (event.key === "Enter" && !event.shiftKey) {
    event.preventDefault();
    submitMessage();
  }
}

onMounted(async () => {
  updateSessionLocation();
  if (!configOnly) {
    await loadInitialData();
    connect();
    scrollToBottom();
    composerInput.value?.focus();
  }
});

onBeforeUnmount(() => {
  eventSource?.close();
  if (animationFrame) cancelAnimationFrame(animationFrame);
});
</script>

<template>
  <div class="app-shell" :class="{ 'sidebar-visible': sidebarOpen && !configOnly, 'config-only': configOnly }">
    <SessionSidebar
      v-if="!configOnly"
      :sessions="sessions"
      :active-session="session"
      :open="sidebarOpen"
      :busy="busy"
      @close="sidebarOpen = false"
      @create="createSession"
      @select="switchSession"
    />

    <div v-if="sidebarOpen && !configOnly" class="mobile-scrim" @click="sidebarOpen = false"></div>

    <section class="main-shell">
      <header class="topbar">
        <div class="topbar-left">
          <button v-if="!configOnly" class="icon-button sidebar-trigger" type="button" title="显示会话" @click="sidebarOpen = !sidebarOpen">
            <PanelLeftOpen v-if="!sidebarOpen" :size="19" /><Menu v-else :size="19" />
          </button>
          <div class="brand-mark"><BrainCircuit :size="20" /></div>
          <div class="brand-copy"><strong>Yomi</strong><span>Personal Agent</span></div>
        </div>

        <nav v-if="!configOnly" class="view-tabs" aria-label="页面导航">
          <button type="button" :class="{ active: activeView === 'chat' }" @click="activeView = 'chat'"><MessageCircleMore :size="16" />对话</button>
          <button type="button" :class="{ active: activeView === 'trace' }" @click="activeView = 'trace'"><Activity :size="16" />轨迹</button>
          <button type="button" :class="{ active: activeView === 'config' }" @click="activeView = 'config'"><Settings :size="16" />配置</button>
        </nav>

        <div class="connection-state" :class="{ online }"><i></i><span>{{ statusText }}</span></div>
      </header>

      <template v-if="activeView === 'chat' && !configOnly">
        <main ref="messagesViewport" class="conversation-view">
          <div v-if="!messages.length" class="welcome-state">
            <div class="welcome-symbol"><Bot :size="30" /></div>
            <span class="eyebrow">READY WHEN YOU ARE</span>
            <h1>今天想一起完成什么？</h1>
            <p>我可以边思考边调用工具，并把过程收纳在同一条回复里。</p>
            <div class="welcome-suggestions">
              <button type="button" @click="input = '帮我梳理这个项目的架构'; composerInput?.focus()">梳理项目架构</button>
              <button type="button" @click="input = '检查当前工作区有哪些待处理问题'; composerInput?.focus()">检查工作区</button>
              <button type="button" @click="input = '帮我制定今天的开发计划'; composerInput?.focus()">制定开发计划</button>
            </div>
          </div>

          <div v-else class="message-list">
            <ChatMessage v-for="message in messages" :key="message.id" :message="message" @answer="submitMessage" />
          </div>
        </main>

        <footer class="composer-dock">
          <div class="composer-card" :class="{ active: input.length, answering }">
            <textarea
              ref="composerInput"
              v-model="input"
              rows="1"
              :placeholder="composerPlaceholder"
              @input="resizeComposer"
              @keydown="handleComposerKeydown"
            ></textarea>
            <div class="composer-actions">
              <span>{{ answering ? "正在等待你的回答" : "Enter 发送 · Shift + Enter 换行" }}</span>
              <button v-if="activeRun" class="stop-button" type="button" title="停止任务" @click="cancelActiveRun"><Square :size="13" fill="currentColor" /></button>
              <button v-else class="send-button" type="button" :disabled="!canSend" title="发送" @click="submitMessage()"><ArrowUp :size="18" /></button>
            </div>
          </div>
          <small>Yomi 可能会犯错，请核对重要信息。</small>
        </footer>
      </template>

      <TraceWorkspace v-else-if="activeView === 'trace' && !configOnly" :session="session" />
      <ConfigWorkspace v-else />
    </section>
  </div>
</template>
