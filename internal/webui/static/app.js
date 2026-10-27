"use strict";

(() => {
  const autoRefreshAfter = 60 * 1000;

  const errorMessages = {
    network: "无法连接到控制服务，请检查网络后重试。",
    invalid_request: "请求无效，请刷新页面后重试。",
    not_found: "控制服务不支持该操作，请确认服务版本。",
    oauth_required: "需要先绑定 eWeLink 账号。",
    oauth_unavailable: "账号凭据暂时不可用，请稍后重试。",
    device_not_found: "找不到该插座，请刷新列表。",
    unsupported_device: "该设备不是受支持的单路插座。",
    device_offline: "插座离线，无法控制。",
    ewelink_rejected: "eWeLink 拒绝了这次请求，请稍后重试。",
    ewelink_unavailable: "暂时无法连接 eWeLink 服务，请稍后重试。",
    internal_error: "控制服务内部错误，请查看服务日志。",
  };

  const $ = (id) => document.getElementById(id);
  const elements = {
    summary: $("summary"),
    updated: $("updated"),
    refresh: $("refresh"),
    notice: $("notice"),
    noticeTitle: $("notice-title"),
    noticeText: $("notice-text"),
    noticeAction: $("notice-action"),
    plates: $("plates"),
    empty: $("empty"),
    template: $("plate-template"),
    footer: $("footer"),
  };

  const entries = new Map();
  let loading = false;
  let lastLoaded = 0;

  class APIError extends Error {
    constructor(code, message) {
      super(message);
      this.code = code;
    }
  }

  async function request(path, options = {}) {
    let response;
    try {
      response = await fetch(path, {
        ...options,
        cache: "no-store",
        headers: { Accept: "application/json", ...options.headers },
      });
    } catch {
      throw new APIError("network", errorMessages.network);
    }
    let body = null;
    try {
      body = await response.json();
    } catch {
      body = null;
    }
    if (!response.ok) {
      const code = body?.error?.code ?? "";
      throw new APIError(code, errorMessages[code] ?? `请求失败（HTTP ${response.status}）。`);
    }
    return body;
  }

  async function loadDevices() {
    if (loading) {
      return;
    }
    loading = true;
    elements.refresh.disabled = true;
    elements.refresh.textContent = "刷新中…";
    try {
      const body = await request("/api/v1/devices");
      const devices = Array.isArray(body?.devices) ? body.devices : [];
      lastLoaded = Date.now();
      showUpdatedTime();
      hideNotice();
      elements.footer.hidden = false;
      render(devices);
    } catch (error) {
      showLoadError(error);
    } finally {
      loading = false;
      elements.refresh.disabled = false;
      elements.refresh.textContent = "刷新";
    }
  }

  function render(devices) {
    const sorted = [...devices].sort((a, b) => displayName(a).localeCompare(displayName(b), "zh-CN"));
    const seen = new Set();
    for (const device of sorted) {
      seen.add(device.device_id);
      let entry = entries.get(device.device_id);
      if (!entry) {
        entry = createEntry();
        entries.set(device.device_id, entry);
      }
      if (!entry.busy) {
        updateEntry(entry, device);
      }
      elements.plates.append(entry.root);
    }
    for (const [id, entry] of entries) {
      if (!seen.has(id)) {
        entry.root.remove();
        entries.delete(id);
      }
    }
    elements.empty.hidden = entries.size > 0;
    summarize();
  }

  function clearDevices() {
    for (const entry of entries.values()) {
      entry.root.remove();
    }
    entries.clear();
    elements.empty.hidden = true;
  }

  function createEntry() {
    const root = elements.template.content.firstElementChild.cloneNode(true);
    const entry = {
      root,
      device: null,
      busy: false,
      copyTimer: 0,
      name: root.querySelector(".plate-name"),
      statusText: root.querySelector(".plate-status-text"),
      message: root.querySelector(".plate-message"),
      rocker: root.querySelector(".rocker"),
      sides: [...root.querySelectorAll(".rocker-side")],
      deviceID: root.querySelector(".device-id"),
      model: root.querySelector(".device-model"),
      url: root.querySelector(".switch-url"),
      copy: root.querySelector(".copy"),
    };
    for (const side of entry.sides) {
      side.addEventListener("click", () => setSwitch(entry, side.dataset.state));
    }
    entry.url.addEventListener("focus", () => entry.url.select());
    entry.copy.addEventListener("click", () => copyURL(entry));
    return entry;
  }

  function updateEntry(entry, device) {
    entry.device = device;
    const name = displayName(device);
    entry.root.dataset.online = String(device.online);
    entry.root.dataset.state = device.online ? device.state : "";
    entry.name.textContent = name;
    entry.statusText.textContent = !device.online ? "离线" : device.state === "on" ? "正在供电" : "已断电";
    entry.rocker.setAttribute("aria-label", `${name} 电源`);
    entry.deviceID.textContent = device.device_id;
    entry.model.textContent = device.model ? `${device.model}（UIID ${device.uiid}）` : `UIID ${device.uiid}`;
    entry.url.value = new URL(`/api/v1/devices/${encodeURIComponent(device.device_id)}/switch`, location.origin).href;
    syncControls(entry);
  }

  function syncControls(entry) {
    const { device, busy } = entry;
    entry.root.dataset.busy = String(busy);
    entry.rocker.setAttribute("aria-busy", String(busy));
    for (const side of entry.sides) {
      side.setAttribute("aria-pressed", String(device.online && device.state === side.dataset.state));
      side.disabled = busy || !device.online;
    }
  }

  async function setSwitch(entry, state) {
    if (entry.busy || !entry.device.online) {
      return;
    }
    entry.busy = true;
    entry.message.textContent = "";
    syncControls(entry);
    const id = entry.device.device_id;
    try {
      await request(`/api/v1/devices/${encodeURIComponent(id)}/switch`, {
        method: "PUT",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ state }),
      });
      entry.busy = false;
      updateEntry(entry, { ...entry.device, state });
    } catch (error) {
      entry.busy = false;
      entry.message.textContent = error.message;
      if (error.code === "device_offline") {
        updateEntry(entry, { ...entry.device, online: false });
      } else {
        syncControls(entry);
      }
      if (error.code === "oauth_required") {
        showOAuthNotice();
      }
    }
    summarize();
  }

  async function copyURL(entry) {
    let copied = false;
    if (navigator.clipboard && window.isSecureContext) {
      try {
        await navigator.clipboard.writeText(entry.url.value);
        copied = true;
      } catch {
        copied = false;
      }
    }
    if (!copied) {
      entry.url.focus();
      entry.url.select();
      try {
        copied = document.execCommand("copy");
      } catch {
        copied = false;
      }
    }
    entry.copy.textContent = copied ? "已复制" : "请手动复制";
    clearTimeout(entry.copyTimer);
    entry.copyTimer = setTimeout(() => {
      entry.copy.textContent = "复制";
    }, 2000);
  }

  function summarize() {
    const devices = [...entries.values()].map((entry) => entry.device);
    if (devices.length === 0) {
      elements.summary.textContent = "没有可控制的插座。";
      return;
    }
    const powered = devices.filter((device) => device.online && device.state === "on").length;
    const offline = devices.filter((device) => !device.online).length;
    let text = `${devices.length} 个插座，${powered} 个正在供电`;
    if (offline > 0) {
      text += `，${offline} 个离线`;
    }
    elements.summary.textContent = `${text}。`;
  }

  function showUpdatedTime() {
    const time = new Date(lastLoaded).toLocaleTimeString("zh-CN", { hour: "2-digit", minute: "2-digit" });
    elements.updated.textContent = `${time} 更新`;
  }

  function showLoadError(error) {
    if (error.code === "oauth_required") {
      clearDevices();
      elements.updated.textContent = "";
      elements.summary.textContent = "未绑定账号";
      showOAuthNotice();
      return;
    }
    elements.summary.textContent = entries.size > 0 ? "插座列表未能更新" : "未能读取插座";
    showNotice("无法读取插座", error.message, false);
  }

  function showOAuthNotice() {
    elements.footer.hidden = true;
    showNotice(
      "尚未绑定 eWeLink 账号",
      "绑定后才能读取和控制插座。授权完成后 eWeLink 会跳转回服务配置的回调地址，请确保这台设备的浏览器能打开该地址；使用默认回调地址时，需要先建立到服务器的 SSH 端口转发。",
      true,
    );
  }

  function showNotice(title, text, withAction) {
    elements.noticeTitle.textContent = title;
    elements.noticeText.textContent = text;
    elements.noticeAction.hidden = !withAction;
    elements.notice.hidden = false;
  }

  function hideNotice() {
    elements.notice.hidden = true;
  }

  function displayName(device) {
    return device.name || device.device_id;
  }

  elements.refresh.addEventListener("click", loadDevices);
  document.addEventListener("visibilitychange", () => {
    if (document.visibilityState === "visible" && Date.now() - lastLoaded > autoRefreshAfter) {
      loadDevices();
    }
  });

  loadDevices();
})();
