<script setup lang="ts">
import { BrainCircuit, Check, ChevronDown, CircleAlert, LoaderCircle, ShieldCheck, Wrench } from "../icons";
import { computed, ref, watch } from "vue";
import type { AgentActivity } from "../types";
import MarkdownContent from "./MarkdownContent.vue";

const props = defineProps<{
  items: AgentActivity[];
  streaming?: boolean;
}>();
const emit = defineEmits<{ answer: [value: string, activityID: string] }>();

const expanded = ref(Boolean(props.streaming));

watch(
  () => props.streaming,
  (streaming) => {
    if (streaming) expanded.value = true;
  },
);

const completedCount = computed(() => props.items.filter((item) => item.status === "completed").length);
const summary = computed(() => {
  if (props.items.some((item) => item.status === "waiting")) return "等待你的确认";
  if (props.streaming) return "正在分析并执行任务";
  const toolCount = props.items.filter((item) => item.kind === "tool").length;
  const hasReasoning = props.items.some((item) => item.kind === "reasoning");
  const parts = [hasReasoning ? "已完成思考" : "", toolCount ? `${toolCount} 次工具调用` : ""].filter(Boolean);
  return parts.join(" · ") || "处理过程";
});

function prettyJSON(value: string): string {
  if (!value) return "";
  try {
    return JSON.stringify(JSON.parse(value), null, 2);
  } catch {
    return value;
  }
}
</script>

<template>
  <section class="activity-panel" :class="{ expanded, streaming }">
    <button class="activity-header" type="button" @click="expanded = !expanded">
      <span class="activity-orb">
        <LoaderCircle v-if="streaming" :size="16" class="spin" />
        <BrainCircuit v-else :size="16" />
      </span>
      <span class="activity-heading">
        <strong>{{ summary }}</strong>
        <small v-if="!streaming">{{ completedCount }}/{{ items.length }} 个步骤</small>
      </span>
      <ChevronDown :size="17" class="activity-chevron" />
    </button>

    <div v-if="expanded" class="activity-list">
      <article v-for="(item, index) in items" :key="item.id" class="activity-item">
        <div class="activity-rail">
          <span class="activity-index">
            <BrainCircuit v-if="item.kind === 'reasoning'" :size="14" />
            <ShieldCheck v-else-if="item.kind === 'approval'" :size="14" />
            <Wrench v-else :size="14" />
          </span>
          <span v-if="index < items.length - 1" class="activity-line"></span>
        </div>
        <div class="activity-content">
          <div class="activity-title-row">
            <strong>{{ item.title }}</strong>
            <span class="activity-status" :class="item.status">
              <LoaderCircle v-if="item.status === 'running'" :size="13" class="spin" />
              <CircleAlert v-else-if="item.status === 'waiting'" :size="13" />
              <CircleAlert v-else-if="item.status === 'failed'" :size="13" />
              <Check v-else :size="13" />
              {{ item.status === "running" ? "进行中" : item.status === "waiting" ? "等待确认" : item.status === "failed" ? "已取消" : "完成" }}
            </span>
          </div>

          <MarkdownContent v-if="item.kind === 'reasoning'" :content="item.content" compact />
          <template v-else-if="item.kind === 'approval'">
            <MarkdownContent :content="item.content" compact />
            <div v-if="item.status === 'waiting' && item.options?.length" class="approval-options">
              <button
                v-for="option in item.options"
                :key="option"
                type="button"
                @click="emit('answer', option, item.id)"
              >
                {{ option }}
              </button>
            </div>
            <div v-if="item.answer" class="approval-answer"><Check :size="13" />你的选择：{{ item.answer }}</div>
          </template>
          <pre v-else-if="item.content" class="activity-code"><code>{{ prettyJSON(item.content) }}</code></pre>

          <details v-if="item.result" class="tool-result">
            <summary>查看工具结果</summary>
            <pre><code>{{ item.result }}</code></pre>
          </details>
        </div>
      </article>
    </div>
  </section>
</template>
