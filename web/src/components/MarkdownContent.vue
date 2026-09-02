<script setup lang="ts">
import DOMPurify from "dompurify";
import { marked } from "marked";
import { computed } from "vue";

const props = defineProps<{
  content: string;
  compact?: boolean;
}>();

const html = computed(() => {
  const rendered = marked.parse(props.content || "", {
    async: false,
    breaks: true,
    gfm: true,
  }) as string;
  return DOMPurify.sanitize(rendered, {
    USE_PROFILES: { html: true },
    ADD_ATTR: ["target", "rel"],
  });
});
</script>

<template>
  <div class="markdown-body" :class="{ compact }" v-html="html"></div>
</template>
