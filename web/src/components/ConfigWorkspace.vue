<script setup lang="ts">
import { Check, Eye, EyeOff, RefreshCw, RotateCcw, Save, Settings2 } from "../icons";
import { computed, onMounted, reactive, ref } from "vue";
import { fetchConfig, resetDebugData, saveConfig } from "../api";
import type { ConfigItem, ConfigView } from "../types";

const config = ref<ConfigView | null>(null);
const values = reactive<Record<string, string>>({});
const visibleSecrets = reactive<Record<string, boolean>>({});
const loading = ref(false);
const saving = ref(false);
const message = ref("");
const error = ref("");
const resetting = ref(false);
const activeSection = ref("");

const sections = computed(() => config.value?.sections ?? []);
const displayedSections = computed(() => sections.value.filter((section) => !activeSection.value || section.id === activeSection.value));

function currentValue(item: ConfigItem): string {
  if (!item.env) return item.value ?? "";
  return values[item.env] ?? "";
}

function setCurrentValue(item: ConfigItem, value: string): void {
  if (item.env) values[item.env] = value;
}

async function load(): Promise<void> {
  loading.value = true;
  error.value = "";
  try {
    config.value = await fetchConfig();
    Object.keys(values).forEach((key) => delete values[key]);
    Object.assign(values, config.value.values ?? {});
    for (const section of config.value.sections ?? []) {
      for (const item of section.items ?? []) {
        if (item.env && values[item.env] === undefined) values[item.env] = item.value ?? "";
      }
    }
    if (!activeSection.value) activeSection.value = config.value.sections?.[0]?.id ?? "";
  } catch (cause) {
    error.value = cause instanceof Error ? cause.message : String(cause);
  } finally {
    loading.value = false;
  }
}

async function save(): Promise<void> {
  saving.value = true;
  error.value = "";
  message.value = "";
  try {
    await saveConfig({ ...values });
    message.value = "配置已保存，重启 Yomi 后生效";
  } catch (cause) {
    error.value = cause instanceof Error ? cause.message : String(cause);
  } finally {
    saving.value = false;
  }
}

async function resetData(): Promise<void> {
  if (resetting.value || !window.confirm("将永久删除所有会话、Trace、Run、Artifact、SQLite 记忆和 Qdrant 向量。确定继续吗？")) return;
  resetting.value = true;
  error.value = "";
  message.value = "";
  try {
    await resetDebugData();
    message.value = "调试数据已清空";
    window.dispatchEvent(new CustomEvent("yomi-data-reset"));
  } catch (cause) {
    error.value = cause instanceof Error ? cause.message : String(cause);
  } finally {
    resetting.value = false;
  }
}

onMounted(load);
</script>

<template>
  <main class="workspace-view config-workspace">
    <section class="workspace-heading">
      <div><span class="eyebrow">CONFIGURATION</span><h1>运行配置</h1><p>管理模型、上下文、长期记忆和工具能力。</p></div>
      <div class="heading-actions">
        <button class="secondary-button" type="button" :disabled="loading" @click="load"><RefreshCw :size="16" :class="{ spin: loading }" />刷新</button>
        <button class="danger-button" type="button" :disabled="resetting" @click="resetData"><RotateCcw :size="16" />{{ resetting ? "清空中" : "清空调试数据" }}</button>
        <button v-if="config?.editable" class="primary-button" type="button" :disabled="saving" @click="save"><Save :size="16" />{{ saving ? "保存中" : "保存配置" }}</button>
      </div>
    </section>

    <div v-if="message" class="inline-success"><Check :size="16" />{{ message }}</div>
    <div v-if="error" class="inline-error">{{ error }}</div>

    <div class="config-layout">
      <nav class="config-nav surface-card">
        <button v-for="section in sections" :key="section.id" type="button" :class="{ active: activeSection === section.id }" @click="activeSection = section.id">
          <Settings2 :size="16" /><span>{{ section.title }}</span>
        </button>
        <small v-if="config?.file">保存位置<br />{{ config.file }}</small>
      </nav>

      <section class="config-sections">
        <article v-for="section in displayedSections" :key="section.id" class="surface-card config-section">
          <header><span class="eyebrow">{{ section.id.toUpperCase() }}</span><h2>{{ section.title }}</h2><p>{{ section.description }}</p></header>
          <div class="config-fields">
            <label v-for="item in section.items" :key="item.key" class="config-field">
              <span class="config-label"><strong>{{ item.key }}</strong><code v-if="item.env">{{ item.env }}</code></span>
              <span class="config-input-wrap" :class="{ readonly: !item.env || !config?.editable }">
                <input
                  :type="item.sensitive && !visibleSecrets[item.key] ? 'password' : 'text'"
                  :value="currentValue(item)"
                  :readonly="!item.env || !config?.editable"
                  :placeholder="item.default || '未设置'"
                  @input="setCurrentValue(item, ($event.target as HTMLInputElement).value)"
                />
                <button v-if="item.sensitive" type="button" title="显示或隐藏密钥" @click.prevent="visibleSecrets[item.key] = !visibleSecrets[item.key]">
                  <EyeOff v-if="visibleSecrets[item.key]" :size="16" /><Eye v-else :size="16" />
                </button>
              </span>
              <span class="config-help">{{ item.description }}</span>
              <span class="config-meta">
                <em v-if="item.status">{{ item.status }}</em><span v-if="item.restart_required"><RotateCcw :size="12" />需重启</span><span v-if="item.default">默认：{{ item.default }}</span>
              </span>
            </label>
          </div>
        </article>
      </section>
    </div>
  </main>
</template>
