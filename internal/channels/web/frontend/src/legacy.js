// Compatibility controller used during the incremental Vue migration.
// The DOM/event behavior remains unchanged while the Vue shell owns mounting.
export function initLegacyController() {
  const list = document.getElementById("list");
  const messages = document.getElementById("messages");
  const input = document.getElementById("input");
  const sendBtn = document.getElementById("send");
  const cancelBtn = document.getElementById("cancel");
  const statusEl = document.getElementById("status");
  const statusDot = document.getElementById("statusDot");
  const messagesView = document.getElementById("messages");
  const composer = document.querySelector("footer");
  const traceView = document.getElementById("traceView");
  const chatTab = document.getElementById("chatTab");
  const traceTab = document.getElementById("traceTab");
  const runSelect = document.getElementById("runSelect");
  const runStatusFilter = document.getElementById("runStatusFilter");
  const traceList = document.getElementById("traceList");
  const traceDetail = document.getElementById("traceDetail");
  const traceStatus = document.getElementById("traceStatus");
  const refreshTrace = document.getElementById("refreshTrace");
  const sessionList = document.getElementById("sessionList");
  const newSessionBtn = document.getElementById("newSession");
  const sessionSidebar = document.getElementById("sessionSidebar");
  const sessionToggle = document.getElementById("sessionToggle");

  // 会话 ID：URL 优先，便于复制链接恢复；localStorage 作为本机默认会话。
  const urlSession = new URLSearchParams(window.location.search).get("session");
  let session = urlSession || localStorage.getItem("szabot_session");
  if (!session) {
    session = "web:" + Date.now() + ":" + Math.random().toString(36).slice(2, 8);
  }
  updateSessionURL();

  let waiting = false;         // 是否在等待回复（禁用发送）
  let answering = false;       // 是否处于"回答 agent 提问"模式
  let activeRun = false;       // 当前 session 是否有一个可由用户取消的 Run
  let activeOptionBtns = [];   // 当前问题的选项按钮，回答后统一禁用
  let traceRuns = [];
  let traceEvents = [];
  let eventSource = null;
  let sessionItems = [];

  function updateSessionURL() {
    const url = new URL(window.location.href);
    url.searchParams.set("session", session);
    window.history.replaceState({}, "", url);
    localStorage.setItem("szabot_session", session);
  }

  function resetConversation() {
    clearPending();
    list.innerHTML = "";
    answering = false;
    waiting = false;
    activeRun = false;
    sendBtn.disabled = false;
    cancelBtn.disabled = true;
    input.placeholder = "给 szabot 发消息…";
  }

  function renderHistory(messages) {
    resetConversation();
    (messages || []).forEach(function (message) {
      const content = message.content || "";
      if (message.role === "user") {
        if (content) addMessage("user", content);
        return;
      }
      if (message.role === "tool") {
        if (content) addCollapsibleMessage("tool", content, "工具结果");
        return;
      }
      if (message.role === "assistant") {
        if (message.reasoning) addCollapsibleMessage("reasoning", message.reasoning, "思考");
        if (content) addMessage("bot", content);
        if (message.toolCalls && message.toolCalls.length) {
          message.toolCalls.forEach(function (call) {
            const args = typeof call.arguments === "string" ? call.arguments : JSON.stringify(call.arguments || {});
            addCollapsibleMessage("tool", (call.name || "工具") + "(" + args + ")", "工具调用");
          });
        }
      }
    });
    if (!list.children.length) {
      list.innerHTML = '<section class="empty-state" aria-label="开始对话"><div class="empty-kicker">YOMI AGENT</div><h2>从一个问题开始</h2><p>实时查看模型思考、工具调用和最终回答。</p><div class="empty-pills" aria-hidden="true"><span>流式响应</span><span>工具调用</span><span>Trace 追踪</span></div></section>';
    }
  }

  async function loadHistory() {
    try {
      const response = await fetch("/api/session/messages?session=" + encodeURIComponent(session));
      if (!response.ok) throw new Error("HTTP " + response.status);
      const data = await response.json();
      renderHistory(data.messages || []);
    } catch (err) {
      resetConversation();
      addMessage("bot", "历史加载失败：" + err.message);
    }
  }

  function renderSessionList(items) {
    sessionItems = items || [];
    sessionList.innerHTML = "";
    const current = sessionItems.find(function (item) { return item.id === session; });
    if (!current) {
      const draft = document.createElement("button");
      draft.className = "session-item active";
      draft.innerHTML = '<span class="session-title">当前会话</span><span class="session-preview">尚未发送消息</span>';
      draft.disabled = waiting;
      sessionList.appendChild(draft);
    }
    sessionItems.forEach(function (item) {
      const button = document.createElement("button");
      button.className = "session-item" + (item.id === session ? " active" : "");
      button.disabled = waiting;
      const title = document.createElement("span");
      title.className = "session-title";
      title.textContent = item.title || item.id;
      const preview = document.createElement("span");
      preview.className = "session-preview";
      preview.textContent = item.preview || "暂无消息";
      button.appendChild(title);
      button.appendChild(preview);
      button.title = item.id;
      button.addEventListener("click", function () { switchSession(item.id); });
      sessionList.appendChild(button);
    });
    if (!sessionList.children.length) {
      sessionList.innerHTML = '<div class="session-list-status">暂无历史会话</div>';
    }
  }

  async function loadSessions() {
    try {
      const response = await fetch("/api/sessions");
      if (!response.ok) throw new Error("HTTP " + response.status);
      const data = await response.json();
      renderSessionList(data.sessions || []);
    } catch (err) {
      sessionList.innerHTML = '<div class="session-list-status">会话加载失败</div>';
    }
  }

  async function switchSession(nextSession) {
    if (!nextSession || nextSession === session) return;
    if (waiting || activeRun || answering) {
      setStatus("当前会话仍在运行", true);
      return;
    }
    if (eventSource) {
      eventSource.close();
      eventSource = null;
    }
    session = nextSession;
    updateSessionURL();
    traceRuns = [];
    traceEvents = [];
    await loadHistory();
    renderSessionList(sessionItems);
    sessionSidebar.classList.remove("open");
    connect();
    if (traceView.style.display !== "none") loadTraceRuns();
  }

  function createSession() {
    if (waiting || activeRun || answering) {
      setStatus("当前会话仍在运行", true);
      return;
    }
    const next = "web:" + Date.now() + ":" + Math.random().toString(36).slice(2, 8);
    switchSession(next);
  }

  function switchView(view) {
    const trace = view === "trace";
    messagesView.style.display = trace ? "none" : "block";
    composer.style.display = trace ? "none" : "block";
    traceView.style.display = trace ? "flex" : "none";
    chatTab.classList.toggle("active", !trace);
    traceTab.classList.toggle("active", trace);
    if (trace) loadTraceRuns();
  }

  function setStatus(text, online) {
    statusEl.textContent = text;
    statusDot.classList.toggle("off", !online);
  }

  function scrollToBottom() {
    messages.scrollTop = messages.scrollHeight;
  }

  function addMessage(role, text, label) {
    const emptyState = list.querySelector(".empty-state");
    if (emptyState) emptyState.remove();
    const msg = document.createElement("div");
    msg.className = "msg " + role;
    const roleEl = document.createElement("div");
    roleEl.className = "role";
    roleEl.textContent = label || (role === "user" ? "你" : "szabot");
    const bubble = document.createElement("div");
    bubble.className = "bubble";
    bubble.textContent = text;
    if (role === "user") {
      msg.appendChild(bubble);
    } else {
      msg.appendChild(roleEl);
      msg.appendChild(bubble);
    }
    list.appendChild(msg);
    scrollToBottom();
    return bubble;
  }

  function addCollapsibleMessage(role, text, label) {
    const emptyState = list.querySelector(".empty-state");
    if (emptyState) emptyState.remove();
    const msg = document.createElement("div");
    msg.className = "msg " + role;
    const details = document.createElement("details");
    details.className = "collapsible";
    const summary = document.createElement("summary");
    summary.textContent = label;
    const bubble = document.createElement("div");
    bubble.className = "bubble";
    bubble.textContent = text;
    details.appendChild(summary);
    details.appendChild(bubble);
    msg.appendChild(details);
    list.appendChild(msg);
    scrollToBottom();
    return bubble;
  }

  // clearPending 收尾所有"进行中"的流式气泡（去掉光标），用于本轮结束或
  // 插入提问卡片时。
  function clearPending() {
    Object.keys(textMessages).forEach(function (id) {
      textMessages[id].classList.remove("pending");
      delete textMessages[id];
    });
    Object.keys(reasoningMessages).forEach(function (id) {
      reasoningMessages[id].classList.remove("pending");
      delete reasoningMessages[id];
    });
  }

  function finishResponse() {
    clearPending();
    waiting = false;
    activeRun = false;
    sendBtn.disabled = false;
    cancelBtn.disabled = true;
    loadSessions();
  }

  // showQuestion 渲染 agent 的提问：一张醒目卡片 + 可选项按钮。
  // 关键：它会立即解禁输入框并聚焦，因为提问期间本轮不会发 RUN_FINISHED ——
  // 工具正阻塞等待回答，用户必须能马上输入/点击来回答。
  function showQuestion(text, options) {
    // 结束当前正在流式的正文/推理气泡，让问题独立成块。
    clearPending();

    const bubble = addMessage("question", text, "需要你的回答");

    activeOptionBtns = [];
    if (options && options.length) {
      const box = document.createElement("div");
      box.className = "options";
      options.forEach(function (opt) {
        const btn = document.createElement("button");
        btn.className = "option-btn";
        btn.textContent = opt;
        btn.addEventListener("click", function () {
          if (!answering) return;
          answerQuestion(opt);
        });
        box.appendChild(btn);
        activeOptionBtns.push(btn);
      });
      bubble.appendChild(box);
    }

    // 进入回答模式：解禁输入，聚焦，等用户输入或点选项。
    answering = true;
    waiting = false;
    activeRun = true;
    sendBtn.disabled = false;
    cancelBtn.disabled = false;
    input.disabled = false;
    input.placeholder = "输入你的回答…";
    input.focus();
    scrollToBottom();
  }

  // answerQuestion 把用户的回答（打字或点按钮）作为一条普通消息发出去，
  // 并把选项按钮全部禁用，避免重复点击。
  function answerQuestion(text) {
    answering = false;
    activeOptionBtns.forEach(function (b) { b.disabled = true; });
    activeOptionBtns = [];
    input.placeholder = "给 szabot 发消息…";
    doSend(text);
  }

  async function cancelTask() {
    if (!activeRun) return;
    cancelBtn.disabled = true;
    try {
      const resp = await fetch("/api/cancel", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ session: session }),
      });
      if (!resp.ok) throw new Error("HTTP " + resp.status);
      clearPending();
      waiting = false;
      answering = false;
      activeRun = false;
      sendBtn.disabled = false;
      input.placeholder = "给 szabot 发消息…";
      addMessage("bot", "任务已取消");
    } catch (err) {
      cancelBtn.disabled = false;
      addMessage("bot", "取消失败：" + err.message);
    }
  }

  const traceLabels = {
    "run.queued": "RUN QUEUED",
    "run.started": "RUN STARTED",
    "run.status.changed": "RUN STATUS",
    "run.finished": "RUN FINISHED",
    "input.received": "INPUT",
    "system.message": "SYSTEM",
    "context.injected": "CONTEXT",
    "context.compacted": "CONTEXT COMPACTED",
    "model.request.started": "MODEL REQUEST",
    "model.response.finished": "MODEL RESPONSE",
    "model.request.failed": "MODEL FAILED",
    "model.status.changed": "MODEL STATUS",
    "assistant.message.completed": "ASSISTANT",
    "tool.execution.started": "TOOL START",
    "tool.execution.finished": "TOOL RESULT",
    "tool.execution.failed": "TOOL FAILED",
    "tool.status.changed": "TOOL STATUS",
    "user.question.asked": "QUESTION",
    "user.question.answered": "ANSWER",
    "memory.retrieval.started": "MEMORY RETRIEVAL START",
    "memory.retrieval.finished": "MEMORY RETRIEVAL",
    "memory.retrieval.failed": "MEMORY RETRIEVAL FAILED",
    "memory.context.injected": "MEMORY CONTEXT",
    "memory.extraction.started": "MEMORY EXTRACTION START",
    "memory.extraction.finished": "MEMORY EXTRACTION",
    "memory.extraction.failed": "MEMORY EXTRACTION FAILED",
    "memory.candidate.accepted": "MEMORY CANDIDATE ACCEPTED",
    "memory.candidate.rejected": "MEMORY CANDIDATE REJECTED",
    "memory.policy.applied": "MEMORY POLICY",
    "memory.write.completed": "MEMORY WRITE",
    "memory.write.failed": "MEMORY WRITE FAILED",
    "memory.index.completed": "MEMORY INDEX",
    "memory.index.failed": "MEMORY INDEX FAILED",
    "memory.deleted": "MEMORY DELETED",
    "memory.rebuild.completed": "MEMORY REBUILD",
  };

  function traceLabel(type) { return traceLabels[type] || type.toUpperCase(); }

  function traceTime(value) {
    if (!value) return "";
    return new Date(value).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit", second: "2-digit" });
  }

  function eventMillis(event) {
    const time = event && Date.parse(event.timestamp);
    return Number.isFinite(time) ? time : 0;
  }

  function nodeDuration(node) {
    const first = node.events[0];
    const last = node.events[node.events.length - 1];
    const explicit = Number(last && last.duration_ms);
    if (Number.isFinite(explicit) && explicit > 0) return explicit;
    const elapsed = eventMillis(last) - eventMillis(first);
    return elapsed > 0 ? elapsed : 1;
  }

  function formatDuration(milliseconds) {
    if (!milliseconds || milliseconds < 1) return "0 ms";
    if (milliseconds < 1000) return Math.round(milliseconds) + " ms";
    return (milliseconds / 1000).toFixed(milliseconds < 10000 ? 1 : 0) + " s";
  }

  function traceSummary(event) {
    const data = event.data || {};
    if (event.type.indexOf("memory.") === 0) {
      if (data.error) return data.error;
      if (event.type === "memory.retrieval.finished" || event.type === "memory.context.injected") return (data.memory_count || 0) + " memories";
      if (event.type === "memory.extraction.finished") return (data.candidate_count || 0) + " candidates";
      if (event.type === "memory.policy.applied") return (data.accepted_count || 0) + " accepted · " + (data.rejected_count || 0) + " rejected";
      if (event.type === "memory.candidate.rejected") return data.reason || ((data.count || 0) + " rejected");
      if (event.type === "memory.candidate.accepted" || event.type === "memory.write.completed") return data.memory_id || data.kind || "accepted";
      if (event.type === "memory.index.completed") return (data.count || 0) + " indexed" + (data.model ? " · " + data.model : "");
      return event.status || "";
    }
    if (data.tool_name) return data.tool_name + (data.result_size ? " · " + data.result_size + " bytes" : "");
    if (data.model) return data.model + (data.model_step ? " · step " + data.model_step : "");
    if (data.error) return data.error;
    if (data.answer) return data.answer;
    if (data.content) return data.content;
    if (data.message) {
      if (data.message.content) return data.message.content;
      if (data.message.role) return data.message.role;
    }
    return event.status || "";
  }

  function traceEventClass(event) {
    return event.type.indexOf("tool.") === 0 ? "tool" : "";
  }

  async function loadTraceRuns() {
    traceStatus.textContent = "加载中…";
    try {
      const status = runStatusFilter.value;
      let url = "/api/runs?session=" + encodeURIComponent(session);
      if (status) url += "&status=" + encodeURIComponent(status);
      const response = await fetch(url);
      if (!response.ok) throw new Error("HTTP " + response.status);
      const data = await response.json();
      traceRuns = data.runs || [];
      runSelect.innerHTML = "";
      if (!traceRuns.length) {
        const empty = document.createElement("option");
        empty.textContent = "当前 Session 暂无 Trace";
        empty.value = "";
        runSelect.appendChild(empty);
        traceList.innerHTML = '<div class="trace-empty">当前 Session 暂无执行轨迹</div>';
        traceDetail.innerHTML = '<div class="trace-empty">完成一次对话后，Trace 会出现在这里</div>';
      } else {
        traceRuns.forEach(function (run) {
          const option = document.createElement("option");
          option.value = run.run_id;
          const inputLabel = run.input_text ? " · " + run.input_text.slice(0, 28) : "";
          option.textContent = (run.status || "run") + " · " + run.run_id.slice(0, 10) + inputLabel + " · " + (run.event_count || 0) + " events";
          runSelect.appendChild(option);
        });
        await loadTraceRun(traceRuns[0].run_id);
      }
      traceStatus.textContent = traceRuns.length + " runs";
    } catch (err) {
      traceStatus.textContent = "加载失败";
      traceList.innerHTML = '<div class="trace-empty">无法读取 Trace：' + err.message + '</div>';
    }
  }

  async function loadTraceRun(runID) {
    if (!runID) return;
    traceStatus.textContent = "读取 Run…";
    try {
      const response = await fetch("/api/traces/run?run_id=" + encodeURIComponent(runID));
      if (!response.ok) throw new Error("HTTP " + response.status);
      const run = await response.json();
      traceEvents = run.events || [];
      renderTraceRun(run);
      traceStatus.textContent = (run.status || "run") + " · " + traceEvents.length + " events";
    } catch (err) {
      traceStatus.textContent = "读取失败";
      traceList.innerHTML = '<div class="trace-empty">无法读取 Run：' + err.message + '</div>';
    }
  }

  function nodeKind(event) {
    if (event.type === "input.received") return "input";
    if (event.type.indexOf("model.") === 0) return "model";
    if (event.type.indexOf("tool.") === 0) return "tool";
    if (event.type === "assistant.message.completed") return "assistant";
    if (event.type.indexOf("context.") === 0) return "context";
    if (event.type === "system.message") return "system";
    if (event.type.indexOf("user.question.") === 0) return "question";
    if (event.type.indexOf("memory.") === 0) return "memory";
    return "run";
  }

  function nodeLabel(kind, eventType) {
    if (kind === "run") return { "run.queued": "QUEUED", "run.started": "STARTED", "run.finished": "FINISHED" }[eventType] || "LIFECYCLE";
    if (kind === "memory") return traceLabels[eventType] || "MEMORY";
    return { input: "USER", model: "MODEL", tool: "TOOL", assistant: "ASSISTANT", context: "CONTEXT", system: "SYSTEM", question: "QUESTION" }[kind] || "EVENT";
  }

  function buildTraceNodes(events) {
    const nodes = [];
    let activeModel = null;
    const activeTools = {};
    let currentStep = null;
    events.forEach(function (event) {
      // Delta 是底层实时输出，完整的 assistant/model/tool 事件已经覆盖它。
      if (event.type === "delta" || event.type === "reasoning" || event.type === "tool_call" || event.type === "tool_result") return;
      const eventData = event.data || {};
      const eventStep = Number(eventData.model_step);
      if (Number.isFinite(eventStep) && eventStep > 0) currentStep = eventStep;
      const kind = nodeKind(event);
      if (event.type === "model.request.started") {
        activeModel = { kind: "model", label: "MODEL", summary: traceSummary(event), events: [event], start: event.timestamp, step: currentStep };
        nodes.push(activeModel);
        return;
      }
      if (event.type === "model.response.finished" || event.type === "model.request.failed") {
        if (activeModel) {
          activeModel.events.push(event);
          activeModel.summary = event.data && event.data.error ? event.data.error : (event.status || activeModel.summary);
        } else {
          nodes.push({ kind: "model", label: "MODEL", summary: traceSummary(event), events: [event], start: event.timestamp, step: currentStep });
        }
        return;
      }
      if (event.type === "tool.execution.started") {
        const data = event.data || {};
        const node = { kind: "tool", label: data.tool_name || "TOOL", summary: traceSummary(event), events: [event], start: event.timestamp, toolID: data.tool_call_id, step: currentStep };
        nodes.push(node);
        if (data.tool_call_id) activeTools[data.tool_call_id] = node;
        return;
      }
      if (event.type === "tool.execution.finished" || event.type === "tool.execution.failed") {
        const data = event.data || {};
        const node = activeTools[data.tool_call_id];
        if (node) {
          node.events.push(event);
          node.summary = data.error || (data.result_size ? data.result_size + " bytes" : event.status || node.summary);
        } else {
          nodes.push({ kind: "tool", label: data.tool_name || "TOOL", summary: traceSummary(event), events: [event], start: event.timestamp, toolID: data.tool_call_id, step: currentStep });
        }
        return;
      }
      if (event.type === "run.queued" || event.type === "run.started" || event.type === "run.finished") {
        nodes.push({ kind: "run", label: nodeLabel(kind, event.type), summary: traceSummary(event), events: [event], start: event.timestamp, step: null });
        return;
      }
      // Memory extraction runs after the model has finished and is not part of a model step.
      const nodeStep = kind === "memory" ? null : currentStep;
      nodes.push({ kind: kind, label: nodeLabel(kind, event.type), summary: traceSummary(event), events: [event], start: event.timestamp, step: nodeStep });
    });
    return nodes;
  }

  function renderTraceRun(run) {
    traceList.innerHTML = "";
    const summary = document.createElement("div");
    summary.className = "run-summary";
    const toolStatuses = Object.keys(run.tool_statuses || {}).map(function (id) {
      return id.slice(0, 8) + ":" + run.tool_statuses[id];
    });
    function addSummaryItem(label, value) {
      const item = document.createElement("span");
      const strong = document.createElement("strong");
      strong.textContent = label;
      item.appendChild(strong);
      item.appendChild(document.createTextNode(" " + value));
      summary.appendChild(item);
    }
    addSummaryItem("Run", run.status || "-");
    if (run.input_text) addSummaryItem("Input", run.input_text);
    addSummaryItem("Model", run.model_status || "-");
    addSummaryItem("Tools", toolStatuses.length ? toolStatuses.join(", ") : "-");
    addSummaryItem("Calls", (run.model_calls || 0) + " model / " + (run.tool_calls || 0) + " tool");
    if (run.status_reason) addSummaryItem("Reason", run.status_reason);
    if (run.trace_warning) addSummaryItem("Trace", run.trace_warning);
    else if (run.trace_complete) addSummaryItem("Trace", "complete");
    traceList.appendChild(summary);
    const nodes = buildTraceNodes(traceEvents);
    const overview = document.createElement("div");
    overview.className = "trace-overview";
    const timedNodes = nodes.filter(function (node) { return node.kind === "input" || node.kind === "model" || node.kind === "tool" || node.kind === "memory"; });
    const starts = timedNodes.map(function (node) { return eventMillis(node.events[0]); }).filter(Boolean);
    const ends = timedNodes.map(function (node) { return eventMillis(node.events[node.events.length - 1]) + nodeDuration(node); }).filter(Boolean);
    const timelineStart = starts.length ? Math.min.apply(null, starts) : 0;
    const timelineEnd = ends.length ? Math.max.apply(null, ends) : timelineStart + 1;
    const timelineSpan = Math.max(1, timelineEnd - timelineStart);

    ["input", "model", "tool", "memory"].forEach(function (kind) {
      const lane = document.createElement("div"); lane.className = "trace-lane";
      const kindNodes = nodes.filter(function (node) { return node.kind === kind; });
      const label = document.createElement("div"); label.className = "lane-label"; label.textContent = kind === "input" ? "Input" : kind === "model" ? "Model" : kind === "tool" ? "Tools" : "Memory";
      label.title = kindNodes.length ? kindNodes.length + " events · " + formatDuration(kindNodes.reduce(function (sum, node) { return sum + nodeDuration(node); }, 0)) : "No events";
      const track = document.createElement("div"); track.className = "lane-track";
      kindNodes.forEach(function (node) {
        const block = document.createElement("button"); block.className = "lane-block " + kind;
        const start = eventMillis(node.events[0]);
        const duration = nodeDuration(node);
        const left = Math.max(0, ((start - timelineStart) / timelineSpan) * 100);
        const width = Math.max(0.8, (duration / timelineSpan) * 100);
        block.style.left = left + "%";
        block.style.width = Math.min(width, 100 - left) + "%";
        block.title = node.label + " · " + formatDuration(duration) + (node.summary ? " · " + node.summary : "");
        block.addEventListener("click", function () { selectTraceNode(node); });
        track.appendChild(block);
      });
      lane.appendChild(label); lane.appendChild(track); overview.appendChild(lane);
    });
    traceList.appendChild(overview);

    const groups = [];
    const groupsByStep = {};
    nodes.forEach(function (node) {
      const key = node.step == null ? "lifecycle" : String(node.step);
      let group = groupsByStep[key];
      if (!group) {
        group = { key: key, step: node.step, nodes: [] };
        groupsByStep[key] = group;
        groups.push(group);
      }
      group.nodes.push(node);
    });
    groups.forEach(function (group) {
      const section = document.createElement("section"); section.className = "trace-step-group";
      const heading = document.createElement("div"); heading.className = "trace-step-heading";
      const title = document.createElement("span"); title.textContent = group.step == null ? "Run lifecycle" : "Model step " + group.step;
      const count = document.createElement("small"); count.textContent = group.nodes.length + " nodes";
      heading.appendChild(title); heading.appendChild(count); section.appendChild(heading);
      group.nodes.forEach(function (node) {
        const button = document.createElement("button"); button.className = "trace-node " + node.kind;
        const type = document.createElement("span"); type.className = "node-type"; type.textContent = node.label;
        const summary = document.createElement("span"); summary.className = "node-summary"; summary.textContent = node.summary || "";
        const timestamp = document.createElement("span"); timestamp.className = "node-time"; timestamp.textContent = traceTime(node.start);
        button.appendChild(type); button.appendChild(summary); button.appendChild(timestamp);
        button.addEventListener("click", function () { selectTraceNode(node, button); });
        section.appendChild(button);
      });
      traceList.appendChild(section);
    });
    if (nodes.length) selectTraceNode(nodes[nodes.length - 1]);
  }

  function selectTraceNode(node, button) {
    document.querySelectorAll(".trace-node.selected, .lane-block.selected").forEach(function (item) { item.classList.remove("selected"); });
    if (button) button.classList.add("selected");
    renderTraceDetail(node);
  }

  function renderTraceDetail(node) {
    const event = node.events[node.events.length - 1];
    const data = event.data || {};
    traceDetail.innerHTML = "";
    const kicker = document.createElement("div");
    kicker.className = "detail-kicker";
    kicker.textContent = node.kind === "run" ? "RUN" : node.label;
    const title = document.createElement("h2");
    title.className = "detail-title";
    title.textContent = node.kind === "run" ? "RUN · " + node.label : node.label;
    const grid = document.createElement("dl");
    grid.className = "detail-grid";
    [["Status", event.status || "-"], ["Events", node.events.length], ["Time", node.start], ["Duration", event.duration_ms ? event.duration_ms + " ms" : "-"]].forEach(function (item) {
      const key = document.createElement("dt"); key.textContent = item[0];
      const value = document.createElement("dd"); value.textContent = item[1];
      grid.appendChild(key); grid.appendChild(value);
    });
    traceDetail.appendChild(kicker);
    traceDetail.appendChild(title);
    traceDetail.appendChild(grid);

    if (node.kind === "model") {
      const request = node.events.find(function (item) { return item.type === "model.request.started"; });
      const requestData = request && request.data || {};
      appendDetailCode("Messages", requestData.messages || []);
      const tools = requestData.tool_definitions || [];
      const toolsSection = document.createElement("section");
      toolsSection.className = "detail-section";
      const toolsHeading = document.createElement("h3"); toolsHeading.textContent = "Tools · " + tools.length;
      toolsSection.appendChild(toolsHeading);
      if (!tools.length) {
        const empty = document.createElement("div"); empty.className = "trace-empty"; empty.textContent = "本轮没有暴露工具"; empty.style.padding = "12px 0"; toolsSection.appendChild(empty);
      }
      tools.forEach(function (tool) {
        const details = document.createElement("details");
        const summary = document.createElement("summary"); summary.textContent = tool.name || "unnamed tool";
        const code = document.createElement("pre"); code.className = "detail-code"; code.textContent = JSON.stringify({ description: tool.description, parameters: tool.parameters }, null, 2);
        details.appendChild(summary); details.appendChild(code); toolsSection.appendChild(details);
      });
      traceDetail.appendChild(toolsSection);
      appendDetailCode("Timing", { model_step: requestData.model_step, streaming: requestData.streaming, duration_ms: event.duration_ms, usage: data.usage, time_to_first_token_ms: data.time_to_first_token_ms });
    }
    if (node.kind === "tool") {
      const started = node.events.find(function (item) { return item.type === "tool.execution.started"; });
      const finished = node.events.find(function (item) { return item.type.indexOf("tool.execution.finished") === 0 || item.type.indexOf("tool.execution.failed") === 0; });
      appendDetailCode("Arguments", started && started.data ? { tool_call_id: started.data.tool_call_id, tool_name: started.data.tool_name, arguments: started.data.arguments } : {});
      appendDetailCode("Result", finished && finished.data ? { result: finished.data.result, error: finished.data.error, result_size: finished.data.result_size } : {});
    }
    appendDetailCode("Payload", node.events.map(function (item) { return { type: item.type, status: item.status, data: item.data }; }));
  }

  function appendDetailCode(title, value) {
    const section = document.createElement("section");
    section.className = "detail-section";
    const heading = document.createElement("h3"); heading.textContent = title;
    const code = document.createElement("pre"); code.className = "detail-code"; code.textContent = JSON.stringify(value, null, 2);
    section.appendChild(heading); section.appendChild(code); traceDetail.appendChild(section);
  }

  // 建立 SSE 连接，接收 AG-UI 事件流。
  //
  // 服务端把内部分片翻译成标准 AG-UI 事件（每条 data 里带 type）：
  //   SESSION / RUN_STARTED / RUN_FINISHED
  //   TEXT_MESSAGE_START / TEXT_MESSAGE_CONTENT / TEXT_MESSAGE_END
  //   REASONING_MESSAGE_START / REASONING_MESSAGE_CONTENT / REASONING_MESSAGE_END
  //   TOOL_CALL_START / TOOL_CALL_ARGS / TOOL_CALL_END / TOOL_CALL_RESULT
  //   CUSTOM (name=ASK_USER_QUESTION)
  // 前端是"哑渲染"：按 type 分发，用 messageId / toolCallId 把同一逻辑流的
  // 事件归到同一个 DOM 元素上。
  function connect() {
    if (eventSource) eventSource.close();
    const es = new EventSource("/api/stream?session=" + encodeURIComponent(session));
    eventSource = es;

    es.onmessage = function (e) {
      let evt;
      try { evt = JSON.parse(e.data); } catch (_) { return; }
      handleAguiEvent(evt);
    };

    es.onerror = function () {
      setStatus("连接断开，重连中…", false);
      // EventSource 会自动重连，这里只更新状态。
    };
  }

  // textMessages / reasoningMessages / toolCalls 记录进行中的 AG-UI 逻辑流到
  // 其 DOM 气泡的映射（按 messageId / toolCallId 归组）。
  const textMessages = {};
  const reasoningMessages = {};
  const toolCalls = {};

  function handleAguiEvent(evt) {
    switch (evt.type) {
      case "SESSION":
        setStatus("已连接", true);
        break;

      case "RUN_STARTED":
        // 新一轮开始。
        break;

      case "TEXT_MESSAGE_START":
        textMessages[evt.messageId] = addMessage("bot", "");
        textMessages[evt.messageId].classList.add("pending");
        break;
      case "TEXT_MESSAGE_CONTENT": {
        let b = textMessages[evt.messageId];
        if (!b) { b = addMessage("bot", ""); b.classList.add("pending"); textMessages[evt.messageId] = b; }
        b.textContent += evt.delta || "";
        scrollToBottom();
        break;
      }
      case "TEXT_MESSAGE_END":
        if (textMessages[evt.messageId]) {
          textMessages[evt.messageId].classList.remove("pending");
          delete textMessages[evt.messageId];
        }
        break;

      case "REASONING_MESSAGE_START":
        reasoningMessages[evt.messageId] = addCollapsibleMessage("reasoning", "", "思考");
        reasoningMessages[evt.messageId].classList.add("pending");
        break;
      case "REASONING_MESSAGE_CONTENT": {
        let b = reasoningMessages[evt.messageId];
        if (!b) { b = addCollapsibleMessage("reasoning", "", "思考"); b.classList.add("pending"); reasoningMessages[evt.messageId] = b; }
        b.textContent += evt.delta || "";
        scrollToBottom();
        break;
      }
      case "REASONING_MESSAGE_END":
        if (reasoningMessages[evt.messageId]) {
          reasoningMessages[evt.messageId].classList.remove("pending");
          delete reasoningMessages[evt.messageId];
        }
        break;

      case "TOOL_CALL_START":
        toolCalls[evt.toolCallId] = addCollapsibleMessage("tool", evt.toolCallName + "(", "工具调用");
        break;
      case "TOOL_CALL_ARGS": {
        const b = toolCalls[evt.toolCallId];
        if (b) b.textContent += (evt.delta || "");
        break;
      }
      case "TOOL_CALL_END": {
        const b = toolCalls[evt.toolCallId];
        if (b) b.textContent += ")";
        break;
      }
      case "TOOL_CALL_RESULT":
        addCollapsibleMessage("tool", evt.content || "（空结果）", "工具结果");
        scrollToBottom();
        break;

      case "CUSTOM":
        if (evt.name === "ASK_USER_QUESTION" && evt.value) {
          showQuestion(evt.value.question || "", evt.value.options || []);
        }
        break;

      case "RUN_FINISHED":
        finishResponse();
        break;
    }
  }

  async function send() {
    const text = input.value.trim();
    if (!text) return;
    // 回答模式：把输入当作对 agent 提问的回答（会禁用选项按钮）。
    if (answering) {
      input.value = "";
      input.style.height = "auto";
      answerQuestion(text);
      return;
    }
    if (waiting) return;
    input.value = "";
    input.style.height = "auto";
    doSend(text);
  }

  // doSend 真正把一条文本发到后端，并进入"等待回复"状态。
  async function doSend(text) {
    addMessage("user", text);
    waiting = true;
    activeRun = true;
    sendBtn.disabled = true;
    cancelBtn.disabled = false;
    renderSessionList(sessionItems);

    try {
      const resp = await fetch("/api/send", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ session: session, text: text }),
      });
      if (!resp.ok) throw new Error("HTTP " + resp.status);
    } catch (err) {
      addMessage("bot", "发送失败：" + err.message);
      waiting = false;
      activeRun = false;
      sendBtn.disabled = false;
      cancelBtn.disabled = true;
    }
  }

  // 自适应高度。
  input.addEventListener("input", function () {
    input.style.height = "auto";
    input.style.height = Math.min(input.scrollHeight, 160) + "px";
  });

  sendBtn.addEventListener("click", send);
  cancelBtn.addEventListener("click", cancelTask);
  newSessionBtn.addEventListener("click", createSession);
  sessionToggle.addEventListener("click", function () { sessionSidebar.classList.toggle("open"); });
  chatTab.addEventListener("click", function () { switchView("chat"); });
  traceTab.addEventListener("click", function () { switchView("trace"); });
  runSelect.addEventListener("change", function () { loadTraceRun(runSelect.value); });
  runStatusFilter.addEventListener("change", loadTraceRuns);
  refreshTrace.addEventListener("click", loadTraceRuns);

  loadSessions();
  loadHistory().finally(connect);
  input.focus();
}
