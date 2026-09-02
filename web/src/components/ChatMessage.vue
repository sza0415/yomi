<script setup lang="ts">
import { Bot, CircleHelp, UserRound } from "../icons";
import type { ChatMessage } from "../types";
import AgentActivity from "./AgentActivity.vue";
import MarkdownContent from "./MarkdownContent.vue";

defineProps<{ message: ChatMessage }>();
const emit = defineEmits<{ answer: [value: string, activityID?: string] }>();
</script>

<template>
  <article class="message-row" :class="`message-${message.role}`">
    <div v-if="message.role !== 'user'" class="message-avatar" :class="message.role">
      <CircleHelp v-if="message.role === 'question'" :size="18" />
      <Bot v-else :size="18" />
    </div>

    <div class="message-column">
      <div v-if="message.role !== 'user'" class="message-author">
        <span>{{ message.role === "question" ? "需要你的回答" : message.role === "notice" ? "Yomi" : "Yomi Agent" }}</span>
        <span v-if="message.streaming" class="stream-indicator"><i></i>生成中</span>
      </div>

      <div class="message-card">
        <AgentActivity
          v-if="message.activities.length"
          :items="message.activities"
          :streaming="message.streaming"
          @answer="(value, activityID) => emit('answer', value, activityID)"
        />
        <MarkdownContent v-if="message.content" :content="message.content" />
        <span v-else-if="message.streaming" class="typing-cursor" aria-label="正在生成"></span>

        <div v-if="message.options?.length" class="question-options">
          <button
            v-for="option in message.options"
            :key="option"
            type="button"
            :disabled="message.optionsDisabled"
            @click="emit('answer', option)"
          >
            {{ option }}
          </button>
        </div>
      </div>
    </div>

    <div v-if="message.role === 'user'" class="message-avatar user">
      <UserRound :size="18" />
    </div>
  </article>
</template>
