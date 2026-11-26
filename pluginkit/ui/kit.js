// The shared runtime of the store's Management Center pages. pluginkit/ui
// inlines it ahead of each page's own scripts, so a page reads everything
// here through the one global it defines, `kit`. Without a DOM (page logic
// tests under node:vm) only the pure helpers run.
"use strict";

globalThis.kit = (() => {
  const MC_AUTH_KEY = "cli-proxy-auth";
  const MC_THEME_KEY = "cli-proxy-theme";
  const REJECTED_MESSAGE =
    "Management key rejected. Five wrong keys block this address for 30 minutes.";
  const hasDOM = typeof document !== "undefined";

  // Pages are served at <base>/v0/resource/plugins/<id>/<file>, where <base>
  // is empty unless a reverse proxy adds a path prefix.
  const pagePath = hasDOM ? location.pathname : "";
  const resource = /\/v0\/resource\/plugins\/([^/]+)\/[^/]*$/.exec(pagePath);
  const base = resource ? pagePath.slice(0, resource.index) : "";
  const pluginId = resource ? resource[1] : "";
  const paths = Object.freeze({
    base,
    plugin: base + "/v0/management/" + pluginId,
    cpa: base + "/v8/management",
    pluginConfig: base + "/v8/management/config/plugins/configs/" + pluginId,
  });

  // Storage can throw in sandboxed frames and private windows.
  const storage = Object.freeze({
    get(area, key) {
      try {
        return area.getItem(key);
      } catch (_) {
        return null;
      }
    },
    set(area, key, value) {
      try {
        area.setItem(key, value);
      } catch (_) {}
    },
    remove(area, key) {
      try {
        area.removeItem(key);
      } catch (_) {}
    },
  });

  const $ = (id) => document.getElementById(id);

  // h builds an element: props starting with "on" add listeners, true sets
  // an empty attribute, and false, null or undefined skip it. Children may
  // be nested arrays, nodes or text.
  function h(tag, props = {}, ...children) {
    const el = document.createElement(tag);
    for (const [key, value] of Object.entries(props || {})) {
      if (value === false || value === undefined || value === null) continue;
      if (key.startsWith("on")) el.addEventListener(key.slice(2), value);
      else el.setAttribute(key, value === true ? "" : String(value));
    }
    for (const child of children.flat(Infinity))
      if (child !== null && child !== undefined && child !== false)
        el.append(
          child instanceof Node
            ? child
            : document.createTextNode(String(child)),
        );
    return el;
  }

  // Theme: follow Management Center's own setting. Its document carries
  // data-theme ("dark", "white", or none for its light theme); when the page
  // cannot reach it, read the persisted setting instead.
  function managementCenterTheme() {
    try {
      if (window.parent !== window)
        return (
          window.parent.document.documentElement.getAttribute("data-theme") ||
          "light"
        );
    } catch (_) {}
    let saved = "auto";
    try {
      const persisted = JSON.parse(storage.get(localStorage, MC_THEME_KEY));
      saved = persisted?.state?.theme || saved;
    } catch (_) {}
    if (saved !== "auto") return saved;
    return matchMedia("(prefers-color-scheme: dark)").matches
      ? "dark"
      : "white";
  }

  function applyTheme() {
    document.documentElement.setAttribute(
      "data-theme",
      managementCenterTheme(),
    );
  }

  function followTheme() {
    applyTheme();
    matchMedia("(prefers-color-scheme: dark)").addEventListener(
      "change",
      applyTheme,
    );
    window.addEventListener("storage", (event) => {
      if (event.key === MC_THEME_KEY) applyTheme();
    });
    try {
      if (window.parent !== window)
        new MutationObserver(applyTheme).observe(
          window.parent.document.documentElement,
          { attributes: true, attributeFilter: ["data-theme"] },
        );
    } catch (_) {}
  }

  // Management Center persists its connection, when "remember" is on, as
  // zustand state {"state":{"apiBase","managementKey"}}, obfuscated by its
  // src/utils/encryption.ts. A key typed on a page lives in sessionStorage.
  function deobfuscate(payload) {
    const prefix = "enc::v1::";
    if (!payload.startsWith(prefix)) return payload;
    const binary = atob(payload.slice(prefix.length));
    const secret = new TextEncoder().encode(
      "cli-proxy-api-webui::secure-storage|" +
        location.host +
        "|" +
        navigator.userAgent,
    );
    const bytes = new Uint8Array(binary.length);
    for (let i = 0; i < binary.length; i++)
      bytes[i] = binary.charCodeAt(i) ^ secret[i % secret.length];
    return new TextDecoder().decode(bytes);
  }

  // readManagementCenter returns the parsed value Management Center stored
  // under key in localStorage, or null.
  function readManagementCenter(key) {
    const raw = storage.get(localStorage, key);
    if (!raw) return null;
    try {
      return JSON.parse(deobfuscate(raw));
    } catch (_) {
      return null;
    }
  }

  function managementCenterState() {
    const state = readManagementCenter(MC_AUTH_KEY)?.state;
    return state && typeof state === "object" ? state : {};
  }

  class AuthError extends Error {}

  class ApiError extends Error {
    constructor(message, status) {
      super(message);
      this.status = status;
    }
  }

  const sessionKey = pluginId + ".management-key";
  const rejectedKey = pluginId + ".rejected-mc-auth";
  const rejectedListeners = [];

  const auth = {
    key: "",
    // "session" for a key typed on the page, "mc" for Management Center's.
    source: "",

    resolve() {
      const typed = storage.get(sessionStorage, sessionKey);
      if (typed) return this.set(typed, "session");
      const raw = storage.get(localStorage, MC_AUTH_KEY);
      const key = managementCenterState().managementKey;
      if (
        raw &&
        raw !== storage.get(sessionStorage, rejectedKey) &&
        typeof key === "string" &&
        key.trim()
      )
        return this.set(key.trim(), "mc");
      return this.set("", "");
    },

    set(key, source) {
      this.key = key;
      this.source = source;
      return this.key;
    },

    use(key) {
      storage.set(sessionStorage, sessionKey, key);
      this.set(key, "session");
    },

    // forget drops the key in use; a rejected Management Center key is not
    // offered again until Management Center stores another one.
    forget() {
      if (this.source === "mc")
        storage.set(
          sessionStorage,
          rejectedKey,
          storage.get(localStorage, MC_AUTH_KEY) || "",
        );
      storage.remove(sessionStorage, sessionKey);
      this.set("", "");
    },

    onRejected(listener) {
      rejectedListeners.push(listener);
    },

    // ask shows the #auth prompt with an optional message.
    ask(message) {
      const error = $("auth-error");
      $("auth").hidden = false;
      if (error) {
        error.textContent = message || "";
        error.hidden = !message;
      }
      $("auth-key").focus();
    },

    // init wires the page's #auth form (#auth-key, optional #auth-error):
    // it calls onReady once a key is available and asks again whenever the
    // host rejects the key.
    init(onReady) {
      const form = $("auth");
      const input = $("auth-key");
      const ask = (message) => this.ask(message);
      form.addEventListener("submit", (event) => {
        event.preventDefault();
        const key = input.value.trim();
        if (!key) return;
        this.use(key);
        input.value = "";
        form.hidden = true;
        onReady();
      });
      this.onRejected(ask);
      if (this.resolve()) onReady();
      else ask("");
    },
  };

  // request calls a management route with the key in use. CPA's config API
  // answering exactly {"error":"not_found"} for an absent node returns
  // `missing` when one is given; any other 404, or a body that is not JSON,
  // say from a proxy in front of CPA, throws, so a save never mistakes an
  // unreadable list for an empty one. A rejected key throws AuthError after
  // the page's rejection listeners run; other failures throw ApiError.
  async function request(url, { method = "GET", body, missing } = {}) {
    if (!auth.key) throw new AuthError("No management key.");
    const headers = { Authorization: "Bearer " + auth.key };
    if (body !== undefined) headers["Content-Type"] = "application/json";
    let response;
    try {
      response = await fetch(url, {
        method,
        headers,
        body: body === undefined ? undefined : JSON.stringify(body),
        cache: "no-store",
        credentials: "omit",
      });
    } catch (err) {
      throw new Error("CLIProxyAPI did not answer: " + err.message);
    }
    let data = null;
    let unreadable = false;
    try {
      // Every CPA and plugin management route answers a JSON body.
      data = JSON.parse(await response.text());
    } catch (_) {
      unreadable = true;
    }
    // CPA's config API names the failure in `error` and explains it in
    // `message`; plugin routes answer only `error`.
    const message =
      [data?.message, data?.error].find(
        (text) => typeof text === "string" && text,
      ) || "";
    if (response.status === 401) {
      auth.forget();
      for (const listener of rejectedListeners) listener(REJECTED_MESSAGE);
      throw new AuthError(REJECTED_MESSAGE);
    }
    const absent =
      response.status === 404 &&
      data?.error === "not_found" &&
      Object.keys(data).length === 1;
    if (absent && missing !== undefined) return missing;
    if (!response.ok)
      throw new ApiError(
        message || "CLIProxyAPI answered HTTP " + response.status + ".",
        response.status,
      );
    if (unreadable)
      throw new ApiError(
        "CLIProxyAPI answered HTTP " + response.status + " without JSON.",
        response.status,
      );
    return data;
  }

  // api calls one of the plugin's own routes, relative to its prefix.
  const api = (path, options) => request(paths.plugin + path, options);
  // cpa calls CLIProxyAPI's v8 management API.
  const cpa = (path, options) => request(paths.cpa + path, options);

  // toast shows a short-lived .notice at the bottom of the page; kind is
  // "ok", "warn" or "danger".
  function toast(message, kind = "") {
    const el = h(
      "div",
      { class: "notice toast " + kind, role: "status" },
      message,
    );
    document.body.append(el);
    setTimeout(() => el.remove(), 5000);
  }

  if (hasDOM) followTheme();

  return Object.freeze({
    paths,
    storage,
    $,
    h,
    applyTheme,
    readManagementCenter,
    managementCenterState,
    AuthError,
    ApiError,
    auth,
    request,
    api,
    cpa,
    toast,
  });
})();
