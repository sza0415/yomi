<script setup>
import { onMounted } from "vue";
import { initLegacyController } from "./legacy";

onMounted(() => {
  initLegacyController();
});
</script>

<template>
  <div class="app-shell">
    <header>
      <div class="brand">
        <span class="dot" id="statusDot"></span>
        <div class="brand-copy">
          <h1>yomi</h1>
          <span>Agent workspace</span>
        </div>
      </div>
      <div class="view-tabs">
        <button class="view-tab active" id="chatTab">聊天</button>
        <button class="view-tab" id="traceTab">Trace</button>
        <button class="view-tab" id="configTab">配置</button>
      </div>
      <button class="session-toggle" id="sessionToggle" title="显示会话">会话</button>
      <div class="header-meta">
        <span class="model-chip">Local Agent</span>
        <span class="status" id="status">连接中…</span>
      </div>
    </header>

    <aside class="session-sidebar" id="sessionSidebar">
      <div class="sidebar-heading">
        <span>历史会话</span>
        <button class="new-session" id="newSession" title="新建会话">+</button>
      </div>
      <div class="session-list" id="sessionList">
        <div class="session-list-status">加载会话中…</div>
      </div>
    </aside>

    <div id="messages">
      <div class="wrap" id="list">
        <section class="empty-state" aria-label="开始对话">
          <div class="empty-kicker">YOMI AGENT</div>
          <h2>从一个问题开始</h2>
          <p>实时查看模型思考、工具调用和最终回答。</p>
          <div class="empty-pills" aria-hidden="true">
            <span>流式响应</span><span>工具调用</span><span>Trace 追踪</span>
          </div>
        </section>
      </div>
    </div>

    <main id="traceView">
      <div class="trace-toolbar">
        <label for="runSelect">Run</label>
        <select id="runSelect"><option value="">选择一次运行</option></select>
        <label for="runStatusFilter">状态</label>
        <select id="runStatusFilter">
          <option value="">全部</option>
          <option value="running">running</option>
          <option value="waiting_user">waiting_user</option>
          <option value="completed">completed</option>
          <option value="failed">failed</option>
          <option value="cancelled">cancelled</option>
          <option value="timed_out">timed_out</option>
          <option value="budget_exceeded">budget_exceeded</option>
        </select>
        <button id="refreshTrace" title="刷新 Trace">刷新</button>
        <span class="trace-status" id="traceStatus"></span>
      </div>
      <div class="trace-layout">
        <section class="trace-list" id="traceList"><div class="trace-empty">选择 Trace 查看执行轨迹</div></section>
        <aside class="trace-detail" id="traceDetail"><div class="trace-empty">点击左侧事件查看详情</div></aside>
      </div>
    </main>

    <main id="configView">
      <div class="config-toolbar">
        <div>
          <div class="config-kicker">YOMI CONFIGURATION</div>
          <h2>运行配置</h2>
          <p>这里展示本次启动实际生效的配置。个人项目模式会显示完整密钥；修改后请重启 yomi。</p>
        </div>
        <button id="refreshConfig" title="刷新配置">刷新</button>
      </div>
      <div id="configContent" class="config-content"><div class="config-empty">加载配置中…</div></div>
    </main>

    <footer>
      <div class="composer">
        <textarea id="input" rows="1" placeholder="给 szabot 发消息…"></textarea>
        <button id="send">发送</button>
        <button id="cancel" class="cancel-button" disabled>取消</button>
      </div>
      <div class="hint">回答会实时流式显示</div>
    </footer>
  </div>
</template>
