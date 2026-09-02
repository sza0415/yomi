<script setup lang="ts">
import { MessageSquareText, PanelLeftClose, Plus, Search } from "../icons";
import { computed, ref } from "vue";
import type { SessionInfo } from "../types";

const props = defineProps<{
  sessions: SessionInfo[];
  activeSession: string;
  open: boolean;
  busy: boolean;
}>();

const emit = defineEmits<{
  close: [];
  create: [];
  select: [session: string];
}>();

const query = ref("");
const filtered = computed(() => {
  const value = query.value.trim().toLocaleLowerCase();
  if (!value) return props.sessions;
  return props.sessions.filter((item) => `${item.title} ${item.preview}`.toLocaleLowerCase().includes(value));
});

function relativeTime(value: string): string {
  const timestamp = Date.parse(value);
  if (!Number.isFinite(timestamp)) return "";
  const minutes = Math.max(0, Math.round((Date.now() - timestamp) / 60000));
  if (minutes < 1) return "刚刚";
  if (minutes < 60) return `${minutes} 分钟前`;
  if (minutes < 1440) return `${Math.floor(minutes / 60)} 小时前`;
  return `${Math.floor(minutes / 1440)} 天前`;
}
</script>

<template>
  <aside class="session-sidebar" :class="{ open }">
    <div class="sidebar-topline">
      <div>
        <span class="eyebrow">WORKSPACE</span>
        <strong>对话记录</strong>
      </div>
      <button class="icon-button" type="button" title="收起会话" @click="emit('close')">
        <PanelLeftClose :size="18" />
      </button>
    </div>

    <button class="new-chat-button" type="button" :disabled="busy" @click="emit('create')">
      <Plus :size="17" />
      新建对话
    </button>

    <label class="session-search">
      <Search :size="15" />
      <input v-model="query" type="search" placeholder="搜索历史对话" />
    </label>

    <div class="session-scroll">
      <button
        v-for="item in filtered"
        :key="item.id"
        class="session-item"
        :class="{ active: item.id === activeSession }"
        type="button"
        :disabled="busy"
        @click="emit('select', item.id)"
      >
        <MessageSquareText :size="16" />
        <span class="session-copy">
          <strong>{{ item.title || item.id }}</strong>
          <small>{{ item.preview || "暂无消息" }}</small>
        </span>
        <time>{{ relativeTime(item.updated_at) }}</time>
      </button>

      <div v-if="!filtered.length" class="sidebar-empty">暂无匹配的会话</div>
    </div>
  </aside>
</template>
