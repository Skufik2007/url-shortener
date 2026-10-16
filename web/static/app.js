"use strict";

(function () {
  var TOKEN_KEY = "shortener.token";
  var EMAIL_KEY = "shortener.email";
  var API = "/api/v1";
  var SVG_NS = "http://www.w3.org/2000/svg";

  var state = { token: null, email: "", mode: "login", links: [], selected: null };

  function $(id) { return document.getElementById(id); }

  // ---- хранилище токена (localStorage может быть недоступен) ----
  function load() {
    try {
      state.token = localStorage.getItem(TOKEN_KEY);
      state.email = localStorage.getItem(EMAIL_KEY) || "";
    } catch (e) { /* игнорируем */ }
  }
  function save() {
    try {
      if (state.token) {
        localStorage.setItem(TOKEN_KEY, state.token);
        localStorage.setItem(EMAIL_KEY, state.email);
      } else {
        localStorage.removeItem(TOKEN_KEY);
        localStorage.removeItem(EMAIL_KEY);
      }
    } catch (e) { /* игнорируем */ }
  }

  // ---- вспомогательные ----
  function el(tag, props, children) {
    var n = document.createElement(tag);
    if (props) {
      Object.keys(props).forEach(function (k) {
        if (k === "text") n.textContent = props[k];
        else if (k === "class") n.className = props[k];
        else n.setAttribute(k, props[k]);
      });
    }
    (children || []).forEach(function (c) { if (c) n.appendChild(c); });
    return n;
  }
  function svg(tag, attrs) {
    var n = document.createElementNS(SVG_NS, tag);
    Object.keys(attrs || {}).forEach(function (k) {
      if (k === "text") n.textContent = attrs[k]; else n.setAttribute(k, attrs[k]);
    });
    return n;
  }
  function show(node, on) { node.hidden = !on; }
  function setMsg(node, text) { node.textContent = text || ""; show(node, !!text); }
  function pad(n) { return n < 10 ? "0" + n : "" + n; }
  function ymd(d) { return d.getUTCFullYear() + "-" + pad(d.getUTCMonth() + 1) + "-" + pad(d.getUTCDate()); }
  function fmtDate(s) {
    var d = new Date(s);
    return isNaN(d) ? "" : d.toLocaleDateString("ru-RU", { day: "2-digit", month: "2-digit", year: "numeric" });
  }
  function fmtNum(n) { return Number(n || 0).toLocaleString("ru-RU"); }

  var toastTimer = null;
  function toast(text) {
    var t = $("toast");
    t.textContent = text;
    show(t, true);
    clearTimeout(toastTimer);
    toastTimer = setTimeout(function () { show(t, false); }, 1800);
  }

  function api(method, path, body) {
    var headers = {};
    if (body !== undefined) headers["Content-Type"] = "application/json";
    if (state.token) headers["Authorization"] = "Bearer " + state.token;
    return fetch(API + path, {
      method: method,
      headers: headers,
      body: body !== undefined ? JSON.stringify(body) : undefined
    }).then(function (resp) {
      if (resp.status === 204) return null;
      return resp.text().then(function (txt) {
        var data = null;
        try { data = txt ? JSON.parse(txt) : null; } catch (e) { /* не JSON */ }
        if (!resp.ok) {
          if (resp.status === 401 && state.token && path.indexOf("/auth/") !== 0) {
            logout();
          }
          var msg = (data && data.error) || ("Ошибка " + resp.status);
          if (resp.status === 429) msg = "Слишком много запросов, подождите немного";
          var err = new Error(msg);
          err.status = resp.status;
          throw err;
        }
        return data;
      });
    });
  }

  // ---- экраны ----
  function render() {
    var logged = !!state.token;
    show($("auth-view"), !logged);
    show($("app-view"), logged);
    show($("userbox"), logged);
    $("useremail").textContent = state.email;
    if (logged) loadLinks();
  }

  function logout() {
    state.token = null;
    state.email = "";
    state.links = [];
    state.selected = null;
    save();
    show($("stats-card"), false);
    render();
  }

  // ---- вход и регистрация ----
  function setMode(mode) {
    state.mode = mode;
    var login = mode === "login";
    $("tab-login").classList.toggle("active", login);
    $("tab-register").classList.toggle("active", !login);
    $("tab-login").setAttribute("aria-selected", login);
    $("tab-register").setAttribute("aria-selected", !login);
    $("auth-submit").textContent = login ? "Войти" : "Зарегистрироваться";
    $("auth-password").setAttribute("autocomplete", login ? "current-password" : "new-password");
    setMsg($("auth-error"), "");
  }

  function onAuthSubmit(ev) {
    ev.preventDefault();
    var email = $("auth-email").value.trim();
    var password = $("auth-password").value;
    if (!email || !password) { setMsg($("auth-error"), "Введите email и пароль"); return; }
    if (state.mode === "register" && password.length < 8) {
      setMsg($("auth-error"), "Пароль должен быть не короче 8 символов");
      return;
    }
    var btn = $("auth-submit");
    btn.disabled = true;
    setMsg($("auth-error"), "");
    api("POST", "/auth/" + state.mode, { email: email, password: password })
      .then(function (res) {
        state.token = res.token;
        state.email = (res.user && res.user.email) || email;
        save();
        $("auth-password").value = "";
        render();
      })
      .catch(function (e) { setMsg($("auth-error"), e.message); })
      .then(function () { btn.disabled = false; });
  }

  // ---- ссылки ----
  function loadLinks() {
    return api("GET", "/links?limit=100").then(function (res) {
      state.links = (res && res.links) || [];
      renderLinks();
    }).catch(function (e) { toast(e.message); });
  }

  function copyText(text) {
    if (navigator.clipboard && navigator.clipboard.writeText) {
      return navigator.clipboard.writeText(text).then(function () { toast("Скопировано"); },
        function () { fallbackCopy(text); });
    }
    fallbackCopy(text);
    return Promise.resolve();
  }
  function fallbackCopy(text) {
    var ta = el("textarea", { "aria-hidden": "true" });
    ta.value = text;
    document.body.appendChild(ta);
    ta.select();
    try { document.execCommand("copy"); toast("Скопировано"); } catch (e) { toast("Не удалось скопировать"); }
    document.body.removeChild(ta);
  }

  function renderLinks() {
    var body = $("links-body");
    body.textContent = "";
    show($("links-empty"), state.links.length === 0);
    show($("links-table"), state.links.length > 0);
    state.links.forEach(function (l) {
      var tr = el("tr", state.selected === l.code ? { "class": "selected" } : null);

      var shortCell = el("td");
      var wrap = el("div", { "class": "short" });
      var a = el("a", { href: l.short_url, target: "_blank", rel: "noopener", text: l.short_url.replace(/^https?:\/\//, "") });
      var copy = el("button", { "class": "btn btn-sm", type: "button", text: "Копировать", "aria-label": "Копировать " + l.short_url });
      copy.addEventListener("click", function () { copyText(l.short_url); });
      wrap.appendChild(a);
      wrap.appendChild(copy);
      shortCell.appendChild(wrap);
      if (l.title) shortCell.appendChild(el("span", { "class": "link-title", text: l.title }));

      var dest = el("td", { "class": "dest", title: l.original_url, text: l.original_url });
      var clicks = el("td", { "class": "num", text: fmtNum(l.click_count) });
      var created = el("td", { "class": "created", text: fmtDate(l.created_at) });

      var actions = el("td", { "class": "actions" });
      var stat = el("button", { "class": "btn btn-sm", type: "button", text: "Статистика" });
      stat.addEventListener("click", function () { selectLink(l.code); });
      var qr = el("button", { "class": "btn btn-sm", type: "button", text: "QR", "aria-label": "QR-код для " + l.short_url });
      qr.addEventListener("click", function () { openQr(l, qr); });
      var del = el("button", { "class": "btn btn-sm btn-danger", type: "button", text: "Удалить" });
      del.addEventListener("click", function () { removeLink(l.code); });
      actions.appendChild(stat);
      actions.appendChild(document.createTextNode(" "));
      actions.appendChild(qr);
      actions.appendChild(document.createTextNode(" "));
      actions.appendChild(del);

      [shortCell, dest, clicks, created, actions].forEach(function (c) { tr.appendChild(c); });
      body.appendChild(tr);
    });
  }

  // ---- фильтр доменной зоны: .ru, .com, .su, .рф ----
  var ZONES = ["ru", "com", "su", "рф"];
  // Зона, набранная не в той раскладке: «кг» — это «ru» в русской раскладке и т. д.
  var LAYOUT = { "кг": "ru", "сщь": "com", "ыг": "su", "ha": "рф" };

  // zoneError возвращает текст ошибки, если зона похожа на .ru/.com/.su/.рф,
  // но набрана с ошибкой. Другие зоны (.org, .de и т. п.) не проверяются.
  function zoneError(value) {
    var m = /^\s*(?:[a-z][a-z0-9+.-]*:\/\/)?(?:[^\/?#@\s]*@)?([^\/?#:\s]+)/i.exec(value || "");
    if (!m) return "";
    var host = m[1].toLowerCase().replace(/\.$/, "");
    var dot = host.lastIndexOf(".");
    if (dot < 0) return "";
    var zone = host.slice(dot + 1);
    if (ZONES.indexOf(zone) >= 0) return "";
    var fix = guessZone(zone);
    return fix ? "Ошибка в доменной зоне «." + zone + "» — возможно, вы имели в виду «." + fix + "»" : "";
  }

  function guessZone(zone) {
    if (LAYOUT[zone]) return LAYOUT[zone];
    var cleaned = zone.replace(/[^a-zа-яё]/g, "").replace(/(.)\1+/g, "$1"); // без знаков и повторов
    var sorted = zone.split("").sort().join("");
    for (var i = 0; i < ZONES.length; i++) {
      var z = ZONES[i];
      if (cleaned === z) return z;                                            // «ru+», «ruu», «comm»
      if (zone !== "us" && sorted === z.split("").sort().join("")) return z;  // «cmo», «ur», «фр»
    }
    return "";
  }

  function onCreate(ev) {
    ev.preventDefault();
    setMsg($("create-error"), "");
    setMsg($("create-ok"), "");
    var payload = { url: $("f-url").value.trim() };
    var alias = $("f-alias").value.trim();
    var title = $("f-title").value.trim();
    var exp = $("f-expires").value;
    if (!payload.url) { setMsg($("create-error"), "Укажите адрес"); return; }
    var zerr = zoneError(payload.url);
    if (zerr) { setMsg($("create-error"), zerr); $("f-url").focus(); return; }
    if (alias) payload.alias = alias;
    if (title) payload.title = title;
    if (exp) {
      var d = new Date(exp);
      if (isNaN(d)) { setMsg($("create-error"), "Некорректная дата"); return; }
      payload.expires_at = d.toISOString();
    }
    api("POST", "/links", payload).then(function (l) {
      setMsg($("create-ok"), "Готово: " + l.short_url);
      $("create-form").reset();
      copyText(l.short_url);
      return loadLinks();
    }).catch(function (e) { setMsg($("create-error"), e.message); });
  }

  function removeLink(code) {
    if (!window.confirm("Удалить ссылку " + code + "? Переходы по ней перестанут работать.")) return;
    api("DELETE", "/links/" + encodeURIComponent(code)).then(function () {
      if (state.selected === code) {
        state.selected = null;
        show($("stats-card"), false);
      }
      toast("Ссылка удалена");
      return loadLinks();
    }).catch(function (e) { toast(e.message); });
  }

  // ---- QR-код ----
  var qrModal = null;

  function closeQr() {
    if (!qrModal) return;
    var m = qrModal;
    qrModal = null;
    document.removeEventListener("keydown", m.onKey, true);
    if (m.root.parentNode) m.root.parentNode.removeChild(m.root);
    if (m.opener && document.body.contains(m.opener)) m.opener.focus();
  }

  function openQr(link, opener) {
    closeQr();
    var code = encodeURIComponent(link.code);
    var title = el("h2", { id: "qr-title", text: "QR-код" });
    var shortText = el("p", { "class": "qr-url mono", text: link.short_url });
    var img = el("img", {
      "class": "qr-img", width: "256", height: "256",
      src: "/qr/" + code + ".png?size=512", alt: "QR-код для " + link.short_url
    });
    var fail = el("p", { "class": "error qr-fail", text: "Не удалось получить QR-код" });
    fail.hidden = true;
    img.addEventListener("error", function () { show(img, false); show(fail, true); });
    var download = el("a", {
      "class": "btn btn-primary", href: "/qr/" + code + ".png?size=1024",
      download: "qr-" + link.code + ".png", text: "Скачать PNG"
    });
    var closeBtn = el("button", { "class": "btn", type: "button", text: "Закрыть" });
    closeBtn.addEventListener("click", closeQr);
    var hint = el("p", { "class": "qr-hint muted", text: "Отсканируйте камерой телефона. Чтобы ссылка открылась на телефоне, запускайте сервис через «Запустить для телефона.bat»." });

    var dialog = el("div", {
      "class": "modal", role: "dialog", "aria-modal": "true", "aria-labelledby": "qr-title", tabindex: "-1"
    }, [
      title, shortText,
      el("div", { "class": "qr-box" }, [img, fail]),
      el("div", { "class": "qr-actions" }, [download, closeBtn]),
      hint
    ]);
    var root = el("div", { "class": "modal-backdrop" }, [dialog]);
    root.addEventListener("click", function (ev) { if (ev.target === root) closeQr(); });

    function onKey(ev) {
      if (ev.key === "Escape") { ev.preventDefault(); closeQr(); return; }
      if (ev.key !== "Tab") return;
      var f = dialog.querySelectorAll("a[href], button");
      if (!f.length) return;
      var first = f[0], last = f[f.length - 1];
      if (ev.shiftKey && (document.activeElement === first || document.activeElement === dialog)) {
        ev.preventDefault(); last.focus();
      } else if (!ev.shiftKey && document.activeElement === last) {
        ev.preventDefault(); first.focus();
      }
    }
    document.addEventListener("keydown", onKey, true);
    qrModal = { root: root, onKey: onKey, opener: opener };
    document.body.appendChild(root);
    dialog.focus();
  }

  // ---- статистика ----
  function setPeriodDays(days) {
    var now = new Date();
    var to = new Date(Date.UTC(now.getFullYear(), now.getMonth(), now.getDate()));
    var from = new Date(to.getTime() - (days - 1) * 86400000);
    $("p-from").value = ymd(from);
    $("p-to").value = ymd(to);
  }

  function selectLink(code) {
    state.selected = code;
    $("stats-code").textContent = code;
    show($("stats-card"), true);
    if (!$("p-from").value || !$("p-to").value) setPeriodDays(30);
    renderLinks();
    loadStats();
    $("stats-card").scrollIntoView({ behavior: "smooth", block: "start" });
  }

  function loadStats() {
    if (!state.selected) return;
    var code = state.selected;
    var from = $("p-from").value, to = $("p-to").value;
    setMsg($("stats-error"), "");
    if (!from || !to || from > to) { setMsg($("stats-error"), "Проверьте период"); return; }
    api("GET", "/links/" + encodeURIComponent(code) + "/stats?from=" + from + "&to=" + to)
      .then(function (st) {
        if (state.selected !== code) return;
        renderStats(st, from, to);
      })
      .catch(function (e) { setMsg($("stats-error"), e.message); });
  }

  function renderStats(st, from, to) {
    $("t-clicks").textContent = fmtNum(st.total_clicks);
    $("t-unique").textContent = fmtNum(st.unique_visitors);
    drawChart($("chart"), st.by_day || [], from, to);
    drawBars($("top-country"), st.by_country);
    drawBars($("top-referrer"), st.by_referrer);
    drawBars($("top-browser"), st.by_browser);
    drawBars($("top-os"), st.by_os);
    drawBars($("top-device"), st.by_device);
  }

  // График по дням: столбцы в SVG, пропущенные дни — нули.
  function drawChart(host, byDay, from, to) {
    host.textContent = "";
    var counts = {};
    byDay.forEach(function (d) { counts[String(d.day).slice(0, 10)] = d.count; });

    var days = [];
    var cur = new Date(from + "T00:00:00Z");
    var end = new Date(to + "T00:00:00Z");
    while (cur <= end && days.length < 400) {
      var key = ymd(cur);
      days.push({ key: key, count: counts[key] || 0 });
      cur = new Date(cur.getTime() + 86400000);
    }

    var W = 720, H = 220, L = 38, R = 8, T = 10, B = 28;
    var root = svg("svg", { viewBox: "0 0 " + W + " " + H, role: "img", "aria-label": "График переходов по дням" });
    var max = 0;
    days.forEach(function (d) { if (d.count > max) max = d.count; });
    if (max === 0) {
      root.appendChild(svg("text", { x: W / 2, y: H / 2, "text-anchor": "middle", "class": "empty", text: "Нет переходов за выбранный период" }));
      host.appendChild(root);
      return;
    }

    // «Красивый» верх шкалы.
    var step = niceStep(max / 4);
    var top = Math.ceil(max / step) * step;
    var plotW = W - L - R, plotH = H - T - B;
    for (var v = 0; v <= top; v += step) {
      var y = T + plotH - (v / top) * plotH;
      root.appendChild(svg("line", { x1: L, x2: W - R, y1: y, y2: y, "class": v === 0 ? "axis" : "grid" }));
      root.appendChild(svg("text", { x: L - 6, y: y + 4, "text-anchor": "end", "class": "tick", text: String(v) }));
    }

    var slot = plotW / days.length;
    var bw = Math.max(1, Math.min(28, slot * 0.7));
    var labelEvery = Math.max(1, Math.ceil(days.length / 8));
    days.forEach(function (d, i) {
      var h = (d.count / top) * plotH;
      var x = L + i * slot + (slot - bw) / 2;
      var bar = svg("rect", {
        x: x, y: T + plotH - h, width: bw, height: Math.max(h, d.count > 0 ? 1 : 0), rx: 2, "class": "bar"
      });
      bar.appendChild(svg("title", { text: d.key + ": " + d.count }));
      root.appendChild(bar);
      if (i % labelEvery === 0) {
        root.appendChild(svg("text", {
          x: L + i * slot + slot / 2, y: H - 8, "text-anchor": "middle", "class": "tick",
          text: d.key.slice(8, 10) + "." + d.key.slice(5, 7)
        }));
      }
    });
    host.appendChild(root);
  }

  function niceStep(raw) {
    if (raw <= 1) return 1;
    var pow = Math.pow(10, Math.floor(Math.log10(raw)));
    var f = raw / pow;
    var nice = f <= 1 ? 1 : f <= 2 ? 2 : f <= 5 ? 5 : 10;
    return nice * pow;
  }

  // Горизонтальные бары для топов.
  function drawBars(host, items) {
    host.textContent = "";
    items = items || [];
    if (!items.length) {
      host.appendChild(el("p", { "class": "nodata", text: "Нет данных" }));
      return;
    }
    var max = items.reduce(function (m, it) { return Math.max(m, it.count); }, 1);
    var sum = items.reduce(function (s, it) { return s + it.count; }, 0);
    items.forEach(function (it) {
      var pct = sum ? Math.round(it.count / sum * 100) : 0;
      var fill = el("i", { "class": "fill" });
      fill.style.width = (it.count / max * 100) + "%";
      host.appendChild(el("div", { "class": "row" }, [
        el("span", { "class": "k", title: it.key, text: it.key || "—" }),
        el("span", { "class": "v", text: fmtNum(it.count) + " · " + pct + "%" }),
        el("span", { "class": "track" }, [fill])
      ]));
    });
  }

  // ---- инициализация ----
  function init() {
    load();
    $("tab-login").addEventListener("click", function () { setMode("login"); });
    $("tab-register").addEventListener("click", function () { setMode("register"); });
    $("auth-form").addEventListener("submit", onAuthSubmit);
    $("logout").addEventListener("click", logout);
    $("create-form").addEventListener("submit", onCreate);
    // Фильтр прямо при вводе адреса: ошибка в зоне показывается сразу.
    $("f-url").addEventListener("input", function () {
      setMsg($("create-error"), zoneError(this.value));
    });
    $("refresh").addEventListener("click", loadLinks);
    $("p-apply").addEventListener("click", loadStats);
    Array.prototype.forEach.call(document.querySelectorAll(".chip"), function (b) {
      b.addEventListener("click", function () {
        setPeriodDays(parseInt(b.getAttribute("data-days"), 10));
        loadStats();
      });
    });
    setPeriodDays(30);
    render();
  }

  init();
})();
