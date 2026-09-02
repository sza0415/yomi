<script setup lang="ts">
import { Activity, Bot, Clock3, Database, RefreshCw, TerminalSquare, UserRound, Wrench } from "../icons";
import { computed, onMounted, ref, watch } from "vue";
import { fetchRun, fetchRuns } from "../api";
import type { TraceEvent, TraceRun } from "../types";

const props = defineProps<{ session: string }>();

const runs = ref<TraceRun[]>([]);
const selectedRunID = ref("");
const selectedRun = ref<TraceRun | null>(null);
const selectedEvent = ref<TraceEvent | null>(null);
const statusFilter = ref("");
const loading = ref(false);
const error = ref("");

const events = computed(() => selectedRun.value?.events ?? []);

const stats = computed(() => {
  const run = selectedRun.value;
  if (!run) return [];
  return [
    { label: "状态", value: run.status || "-" },
    { label: "模型调用", value: String(run.model_calls ?? 0) },
    { label: "工具调用", value: String(run.tool_calls ?? 0) },
    { label: "事件", value: String(run.event_count ?? events.value.length) },
  ];
});

function eventKind(event: TraceEvent): string {
  if (event.type === "input.received") return "input";
  if (event.type.startsWith("model.")) return "model";
  if (event.type.startsWith("tool.")) return "tool";
  if (event.type.startsWith("memory.")) return "memory";
  return "system";
}

function eventLabel(event: TraceEvent): string {
  const labels: Record<string, string> = {
    "run.queued": "任务已排队",
    "run.started": "任务开始",
    "run.finished": "任务完成",
    "input.received": "收到用户输入",
    "model.request.started": "模型请求",
    "model.response.finished": "模型响应",
    "model.request.failed": "模型失败",
    "tool.execution.started": "工具开始",
    "tool.execution.finished": "工具完成",
    "tool.execution.failed": "工具失败",
    "memory.retrieval.started": "检索长期记忆",
    "memory.retrieval.finished": "记忆检索完成",
    "memory.context.injected": "注入记忆上下文",
    "assistant.message.completed": "回答完成",
  };
  return labels[event.type] || event.type;
}

function eventSummary(event: TraceEvent): string {
  const data = event.data ?? {};
  const preferred = data.tool_name ?? data.model ?? data.error ?? data.content ?? data.answer ?? event.status;
  if (typeof preferred === "string") return preferred;
  if (typeof data.result_size === "number") return `${data.result_size} bytes`;
  return "";
}

function formatTime(value?: string): string {
  if (!value) return "";
  return new Date(value).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit", second: "2-digit" });
}

async function loadRuns(): Promise<void> {
  loading.value = true;
  error.value = "";
  try {
    runs.value = await fetchRuns(props.session, statusFilter.value);
    if (!runs.value.length) {
      selectedRunID.value = "";
      selectedRun.value = null;
      return;
    }
    if (!runs.value.some((run) => run.run_id === selectedRunID.value)) {
      selectedRunID.value = runs.value[0].run_id;
    }
    await loadSelectedRun();
  } catch (cause) {
    error.value = cause instanceof Error ? cause.message : String(cause);
  } finally {
    loading.value = false;
  }
}

async function loadSelectedRun(): Promise<void> {
  if (!selectedRunID.value) return;
  loading.value = true;
  try {
    selectedRun.value = await fetchRun(selectedRunID.value);
    selectedEvent.value = selectedRun.value.events?.at(-1) ?? null;
  } catch (cause) {
    error.value = cause instanceof Error ? cause.message : String(cause);
  } finally {
    loading.value = false;
  }
}

watch(() => props.session, loadRuns);
onMounted(loadRuns);
</script>

<template>
  <main class="workspace-view trace-workspace">
    <section class="workspace-heading">
      <div>
        <span class="eyebrow">OBSERVABILITY</span>
        <h1>运行轨迹</h1>
        <p>检查一次 Agent 运行中的模型、工具与长期记忆活动。</p>
      </div>
      <button class="secondary-button" type="button" :disabled="loading" @click="loadRuns">
        <RefreshCw :size="16" :class="{ spin: loading }" />刷新
      </button>
    </section>

    <section class="trace-controls surface-card">
      <label>
        <span>运行记录</span>
        <select v-model="selectedRunID" @change="loadSelectedRun">
          <option v-if="!runs.length" value="">暂无运行记录</option>
          <option v-for="run in runs" :key="run.run_id" :value="run.run_id">
            {{ run.status || "run" }} · {{ run.input_text || run.run_id.slice(0, 12) }}
          </option>
        </select>
      </label>
      <label>
        <span>状态筛选</span>
        <select v-model="statusFilter" @change="loadRuns">
          <option value="">全部状态</option>
          <option v-for="status in ['running', 'waiting_user', 'completed', 'failed', 'cancelled', 'timed_out', 'budget_exceeded']" :key="status" :value="status">{{ status }}</option>
        </select>
      </label>
      <span class="trace-run-count">{{ runs.length }} runs</span>
    </section>

    <div v-if="error" class="inline-error">{{ error }}</div>

    <template v-if="selectedRun">
      <section class="trace-stats">
        <article v-for="stat in stats" :key="stat.label" class="surface-card trace-stat">
          <small>{{ stat.label }}</small><strong>{{ stat.value }}</strong>
        </article>
      </section>

      <section class="trace-grid">
        <div class="surface-card event-stream">
          <div class="panel-heading"><div><span class="eyebrow">TIMELINE</span><h2>事件流</h2></div><Activity :size="18" /></div>
          <button
            v-for="event in events"
            :key="`${event.sequence}-${event.type}`"
            class="event-row"
            :class="[{ selected: selectedEvent === event }, eventKind(event)]"
            type="button"
            @click="selectedEvent = event"
          >
            <span class="event-icon">
              <UserRound v-if="eventKind(event) === 'input'" :size="15" />
              <Bot v-else-if="eventKind(event) === 'model'" :size="15" />
              <Wrench v-else-if="eventKind(event) === 'tool'" :size="15" />
              <Database v-else-if="eventKind(event) === 'memory'" :size="15" />
              <TerminalSquare v-else :size="15" />
            </span>
            <span class="event-copy"><strong>{{ eventLabel(event) }}</strong><small>{{ eventSummary(event) }}</small></span>
            <time><Clock3 :size="12" />{{ formatTime(event.timestamp) }}</time>
          </button>
          <div v-if="!events.length" class="panel-empty">此 Run 没有可展示的事件</div>
        </div>

        <aside class="surface-card event-detail">
          <div class="panel-heading"><div><span class="eyebrow">INSPECTOR</span><h2>事件详情</h2></div></div>
          <template v-if="selectedEvent">
            <dl class="detail-metadata">
              <div><dt>类型</dt><dd>{{ selectedEvent.type }}</dd></div>
              <div><dt>状态</dt><dd>{{ selectedEvent.status || "-" }}</dd></div>
              <div><dt>耗时</dt><dd>{{ selectedEvent.duration_ms ? `${selectedEvent.duration_ms} ms` : "-" }}</dd></div>
              <div><dt>序号</dt><dd>{{ selectedEvent.sequence ?? "-" }}</dd></div>
            </dl>
            <pre class="detail-code"><code>{{ JSON.stringify(selectedEvent.data ?? {}, null, 2) }}</code></pre>
          </template>
          <div v-else class="panel-empty">选择一个事件查看完整 Payload</div>
        </aside>
      </section>
    </template>

    <div v-else-if="!loading" class="workspace-empty surface-card">完成一次对话后，运行轨迹会显示在这里。</div>
  </main>
</template>
