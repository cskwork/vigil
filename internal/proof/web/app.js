"use strict";
(() => {
  const root = document.getElementById("app"),
    notice = document.getElementById("notice");
  const state = {
    session: null,
    targets: {},
    csrf: "",
    check: null,
    attempts: [],
    selected: null,
    editing: false,
    viewDraft: false,
    busy: false,
    cache: new Map(),
    timer: null,
    last: "",
  };
  const labels = {
    PASS: "요구사항 충족",
    FAIL: "문제 발견",
    INCOMPLETE: "확인 불가",
    UNKNOWN: "확인 불가",
    queued: "실행 대기",
    running: "확인 중",
    cancelled: "실행 취소",
    planning: "기준을 만들고 있습니다",
  };
  const el = (tag, text, cls) => {
    const n = document.createElement(tag);
    if (text !== undefined) n.textContent = text;
    if (cls) n.className = cls;
    return n;
  };
  const add = (p, ...cs) => {
    cs.filter(Boolean).forEach((c) => p.append(c));
    return p;
  };
  const btn = (text, fn, primary = false) => {
    const b = el("button", text, primary ? "primary" : "");
    b.type = "button";
    b.onclick = () => guard(fn, b);
    return b;
  };
  const field = (title, node) => {
    const l = el("label", undefined, "field");
    return add(l, el("span", title), node);
  };
  const badge = (s) =>
    el(
      "span",
      labels[s] || labels[String(s).toLowerCase()] || s || "아직 실행하지 않음",
      "badge " + (s || ""),
    );
  const criterionBadge = (s) =>
    el(
      "span",
      {
        PASS: "충족",
        FAIL: "불일치",
        UNKNOWN: "확인 불가",
        queued: "확인 대기",
      }[s] || "확인 불가",
      "badge " + s,
    );
  const sourceLabel = (s) =>
    ({ dom: "화면", browser: "브라우저", network: "API", mysql: "DB" })[s] || s;
  const reasonText = (r) => {
    if (!r) return "";
    if (/differs|mismatch/i.test(r))
      return "실제값이 예상값과 다릅니다. 아래 관측 근거와 저장 동작을 확인해 주세요.";
    if (/restart/i.test(r))
      return "서버가 다시 시작되어 확인을 마치지 못했습니다. 다시 실행해 주세요.";
    if (/cancel/i.test(r)) return "실행이 취소되어 확인하지 못했습니다.";
    if (/version/i.test(r))
      return "실행 중 배포 버전이 달라졌거나 버전을 확인하지 못했습니다. 배포 상태를 확인해 주세요.";
    if (/expired/i.test(r))
      return "근거의 보관 기간이 지났습니다. 다시 실행해 주세요.";
    if (
      /unavailable|missing|not reached|unsupported|timeout|deadline|not connected/i.test(
        r,
      )
    )
      return "필요한 관측을 완료하지 못했습니다. 서비스와 관측 연결을 확인한 뒤 다시 실행해 주세요.";
    return /[가-힣]/.test(r)
      ? r
      : "확인에 필요한 근거를 확보하지 못했습니다. 상세 사유와 연결 상태를 확인해 주세요.";
  };
  const versionText = (a) => {
    const before = a.observed_version || a.version_before,
      after = a.version_after;
    if (a.version_changed || (before && after && before !== after))
      return "실행 중 배포 변경: " + before + " → " + (after || "미확인");
    return before && after ? before : "배포 버전 미확인";
  };
  const comparisonText = (a) =>
    a.fix_claim === "versioned baseline FAIL to current PASS" &&
    a.baseline_kind === "prior-deployment"
      ? "이전 배포의 불일치가 현재 배포에서 충족으로 바뀐 것을 확인했습니다."
      : a.approval.baseline
        ? "이전 실행이 연결되어 있지만 수정 전후의 개선은 확인되지 않았습니다."
        : "이전 버전 실행 없음";
  const date = (v) =>
    v && !String(v).startsWith("0001")
      ? new Date(v).toLocaleString("ko-KR")
      : "관측 시각 미확인";
  const active = (a) =>
    a &&
    ["queued", "running", "pending"].includes(String(a.state).toLowerCase());
  const val = (v) =>
    !v?.present
      ? "값 없음"
      : typeof v.data === "string"
        ? v.data === ""
          ? '빈 문자열 ("")'
          : v.data
        : JSON.stringify(v.data);
  function message(s) {
    notice.textContent = s;
  }
  async function api(path, method = "GET", body) {
    const headers = {};
    if (method !== "GET") {
      headers["Content-Type"] = "application/json";
      headers["X-Proof-CSRF"] = state.csrf;
    }
    const cached = state.cache.get(path);
    if (method === "GET" && cached?.etag)
      headers["If-None-Match"] = cached.etag;
    let res;
    try {
      res = await fetch("/api/proof" + path, {
        method,
        headers,
        body: body === undefined ? undefined : JSON.stringify(body),
      });
    } catch (e) {
      throw Error(
        "서버와 연결이 끊겼습니다. 입력과 실행 기록은 유지됩니다. 연결을 확인한 뒤 다시 시도해 주세요.",
      );
    }
    if (res.status === 304) return cached.data;
    let data;
    try {
      data = await res.json();
    } catch (e) {
      throw Error("서버 응답을 읽지 못했습니다. 잠시 후 다시 시도해 주세요.");
    }
    if (!res.ok) {
      const hints = {
        409: "다른 곳에서 기준이 변경됐거나 실행 중입니다. 새로고침해 최신 상태를 확인해 주세요.",
        403: "이 작업을 실행할 권한이 없거나 세션이 만료됐습니다. 새로고침 후 다시 시도해 주세요.",
        422: "기준과 예상값을 확인해 주세요.",
      };
      const error = Error(
        (hints[res.status] ||
          "요청을 처리하지 못했습니다. 입력을 확인한 뒤 다시 시도해 주세요.") +
          " " +
          (data.error || ""),
      );
      error.status = res.status;
      throw error;
    }
    if (method === "GET")
      state.cache.set(path, { data, etag: res.headers.get("ETag") });
    else state.cache.clear();
    return data;
  }
  async function guard(fn, b) {
    if (state.busy) return;
    state.busy = true;
    if (b) b.disabled = true;
    try {
      await fn();
    } catch (e) {
      showError(e);
    } finally {
      state.busy = false;
      if (b?.isConnected) b.disabled = false;
    }
  }
  function showError(e) {
    if (e.status === 401 && state.session) {
      state.session = null;
      login();
      message("로그인 시간이 끝났습니다. 다시 로그인하면 같은 화면에서 이어집니다.");
      return;
    }
    message(e.message);
  }
  function clear() {
    root.replaceChildren();
  }
  function readLocal(key) {
    try {
      return localStorage.getItem(key);
    } catch (e) {
      return null;
    }
  }
  function writeLocal(key, v) {
    try {
      localStorage.setItem(key, v);
    } catch (e) {}
  }
  function targetInfo(target, contract) {
    const d = el("dl");
    const persona = target?.personas?.[contract?.persona];
    const entries = [
      ["환경", target?.environment || "환경 미확인"],
      ["대상 URL", target?.base_url || "대상 미확인"],
      ["선택된 계정", persona?.account || contract?.persona || "계정 미확인"],
      [
        "영향을 받는 데이터",
        target?.fixtures?.[contract?.fixture]?.entity ||
          contract?.fixture ||
          "데이터 미확인",
      ],
      [
        "허용된 동작",
        (contract?.actions || [])
          .map((x) => target?.actions?.[x]?.title || x)
          .join(", ") || "등록된 동작 없음",
      ],
    ];
    for (const [k, v] of entries) add(d, el("dt", k), el("dd", v));
    return d;
  }
  function home() {
    clear();
    const heading = el("div", undefined, "page-heading"),
      headingCopy = el("div");
    add(
      headingCopy,
      el("p", "검증 요청", "eyebrow"),
      el("h1", "무엇이 제대로 수정됐는지 확인할까요?"),
      el(
        "p",
        "확인할 내용을 적으면 기준을 만들고, 승인한 범위에서 실제 결과를 확인합니다.",
        "intro muted",
      ),
    );
    heading.append(headingCopy);
    if (state.session?.login) {
      const signed = el("div", undefined, "row session-row");
      add(
        signed,
        el("p", `${state.session.actor} 계정으로 확인 중`, "muted"),
        btn("로그아웃", async () => {
          await api("/logout", "POST", {});
          location.href = "/";
        }),
      );
      heading.append(signed);
    }
    root.append(heading);
    const form = el("form", undefined, "panel"),
      select = el("select");
    select.required = true;
    const entries = Object.entries(state.targets);
    for (const [k, t] of entries) {
      const o = el("option", `${t.title} · ${t.base_url}`);
      o.value = k;
      select.append(o);
    }
    const previous = readLocal("proof-target");
    if (state.targets[previous]) select.value = previous;
    const targetURL = el("input");
    targetURL.type = "url";
    targetURL.required = true;
    targetURL.value =
      readLocal("proof-url") || state.targets[select.value]?.base_url || "";
    const env = el("p", undefined, "muted");
    const update = () => {
      const t = state.targets[select.value];
      env.textContent = t
        ? `환경: ${t.environment} · 계정: ${
            Object.values(t.personas || {})
              .map((p) => p.account)
              .join(", ") || "연결되지 않음"
          }`
        : "등록된 대상이 없습니다. 관리자에게 대상 연결을 요청해 주세요.";
    };
    select.onchange = () => {
      writeLocal("proof-target", select.value);
      targetURL.value = state.targets[select.value]?.base_url || "";
      writeLocal("proof-url", targetURL.value);
      update();
    };
    targetURL.oninput = () => {
      writeLocal("proof-url", targetURL.value);
      const match = entries.find(
        ([, t]) => t.base_url === targetURL.value.trim(),
      );
      if (match) {
        select.value = match[0];
        writeLocal("proof-target", match[0]);
        update();
      }
    };
    update();
    const request = el("textarea");
    request.required = true;
    request.minLength = 3;
    request.maxLength = 10000;
    request.placeholder =
      "예: 제목을 수정하고 저장한 뒤 새로고침해도 수정한 제목이 그대로 보여야 합니다.";
    request.value = readLocal("proof-request") || "";
    request.oninput = () => writeLocal("proof-request", request.value);
    const submit = el("button", "확인 기준 만들기", "primary");
    submit.type = "submit";
    submit.disabled = !entries.length;
    add(
      form,
      field("확인 환경", select),
      field("대상 URL", targetURL),
      env,
      field("무엇이 어떻게 동작해야 하나요?", request),
      submit,
    );
    form.onsubmit = (e) => {
      e.preventDefault();
      guard(async () => {
        const match = entries.find(
          ([, t]) => t.base_url === targetURL.value.trim(),
        );
        if (!match)
          throw Error("이 주소는 아직 확인 대상으로 등록되지 않았습니다.");
        const pendingKey = "proof-intake-pending";
        const payloadKey = JSON.stringify([match[0], request.value]);
        let pending;
        try {
          pending = JSON.parse(sessionStorage.getItem(pendingKey) || "null");
        } catch (e) {}
        if (!pending || pending.payload !== payloadKey) {
          pending = { payload: payloadKey, key: crypto.randomUUID() };
          sessionStorage.setItem(pendingKey, JSON.stringify(pending));
        }
        writeLocal("proof-target", select.value);
        const c = await api("/checks", "POST", {
          target_ref: match[0],
          url: targetURL.value.trim(),
          request: request.value,
          idempotency_key: pending.key,
        });
        sessionStorage.removeItem(pendingKey);
        writeLocal("proof-request", "");
        location.href = "/checks/" + encodeURIComponent(c.id);
      }, submit);
    };
    const section = el("section", undefined, "section recent-panel");
    add(section, el("h2", "최근 확인"), el("div", undefined, "recent"));
    root.append(add(el("div", undefined, "dashboard"), form, section));
    refreshHome();
  }
  async function refreshHome() {
    try {
      const d = await api("/checks");
      const list = root.querySelector(".recent");
      if (!list) return;
      const sig = JSON.stringify(d);
      if (list.dataset.sig === sig) return;
      list.dataset.sig = sig;
      list.replaceChildren();
      if (!d.checks?.length) {
        list.append(
          el(
            "p",
            "아직 확인한 내용이 없습니다. 첫 확인 기준을 만들어 보세요.",
            "empty",
          ),
        );
        return;
      }
      const ul = el("ul", undefined, "history");
      for (const c of d.checks) {
        const total = (c.draft?.criteria || []).filter((x) => x.required).length;
        const coverage = c.latest_verdict
          ? ` · ${Math.max(0, total - c.missing_count)}개 확인${c.missing_count ? " / " + c.missing_count + "개 미확인" : ""}`
          : "";
        const li = el("li"),
          a = el("a", c.request);
        a.href = "/checks/" + encodeURIComponent(c.id);
        add(
          li,
          add(el("div", undefined, "row between"), a, badge(c.latest_verdict)),
          el(
            "p",
            `${state.targets[c.target_ref]?.title || c.target_ref} · ${date(c.updated_at && !String(c.updated_at).startsWith("0001") ? c.updated_at : c.created_at)}${coverage}`,
            "muted",
          ),
        );
        ul.append(li);
      }
      list.append(ul);
    } catch (e) {
      showError(e);
    }
  }
  function inspectDetails(title, data) {
    const d = el("details");
    return add(
      d,
      el("summary", title),
      el("pre", JSON.stringify(data, null, 2)),
    );
  }
  function selectedAttempt() {
    return (
      state.attempts.find((a) => a.id === state.selected) || state.attempts[0]
    );
  }
  function preserveRender(fn) {
    const focused = root.contains(document.activeElement)
      ? document.activeElement
      : null;
    const focusKey = focused
      ? {
          tag: focused.tagName,
          text: focused.textContent,
          href: focused.getAttribute("href"),
        }
      : null;
    const opens = [...root.querySelectorAll("details[open]")]
      .map((x) => x.dataset.key)
      .filter(Boolean);
    fn();
    for (const x of root.querySelectorAll("details"))
      if (opens.includes(x.dataset.key)) x.open = true;
    if (focusKey) {
      const next = [...root.querySelectorAll("button,a,summary,select")].find(
        (x) =>
          x.tagName === focusKey.tag &&
          x.textContent === focusKey.text &&
          x.getAttribute("href") === focusKey.href,
      );
      next?.focus({ preventScroll: true });
    }
  }
  function detail() {
    const c = state.check,
      a = state.viewDraft ? null : selectedAttempt();
    clear();
    const back = el("a", "모든 확인");
    back.href = "/";
    root.append(back);
    add(
      root,
      el("p", "CHECK / " + c.id.slice(0, 8).toUpperCase(), "eyebrow"),
      el("h1", "확인 기준과 결과"),
      el("p", c.request, "check-request"),
    );
    const target = state.targets[c.target_ref] || a?.target || {};
    if (
      c.planning &&
      !["ready", "completed", "done"].includes(c.planning.toLowerCase())
    )
      add(
        root,
        el(
          "p",
          c.plan_error
            ? "기준을 만들지 못했습니다. 아래 연결 정보와 오류를 확인해 주세요."
            : "확인 기준을 만들고 있습니다. 이 화면을 닫아도 작업은 유지됩니다.",
          "notice",
        ),
      );
    if (c.plan_error)
      root.append(inspectDetails("기준 생성 오류", c.plan_error));
    if (c.plan_observation?.status) {
      const observed = el("details");
      observed.dataset.key = "plan-observation";
      add(
        observed,
        el("summary", "기준 생성에 참고한 화면"),
        el("p", c.plan_observation.note || "", "muted"),
        el(
          "p",
          `${c.plan_observation.url || "주소 미확인"} · 컨트롤 ${c.plan_observation.controls?.length || 0}개 · ${date(c.plan_observation.observed_at)}`,
          "muted",
        ),
      );
      root.append(observed);
    }
    if ((c.plan_error || c.question) && !a) renderPlanRecovery(c);
    const scope = el(a ? "details" : "section", undefined, "panel");
    if (a) {
      scope.dataset.key = "scope";
      add(
        scope,
        el("summary", "확인 범위"),
        targetInfo(a.target || target, a.contract),
      );
    } else {
      add(scope, el("h2", "확인 범위"), targetInfo(target, c.draft));
      root.append(scope);
    }
    const contract = a?.contract || c.draft || { criteria: [] };
    const unsupported = (contract.criteria || []).filter(
      (x) => !(target.observers || {})[x.observer],
    );
    if (unsupported.length)
      root.append(
        el(
          "p",
          "연결되지 않은 관측 항목이 있습니다: " +
            unsupported.map((x) => x.title).join(", ") +
            ". 실행해도 해당 기준은 확인 불가로 남습니다.",
          "notice",
        ),
      );
    if (a) {
      renderAttempt(a);
      root.append(scope);
      renderHistory();
    } else renderDraft(c, target);
    const tech = inspectDetails("감사 정보", {
      check_id: c.id,
      attempt_id: a?.id,
      requested_by: c.created_by,
      executed_by: a?.actor,
      approved_at: a?.approved_at,
      revision: c.current_revision,
      contract_hash: c.contract_hash,
      planning: c.planning,
    });
    tech.dataset.key = "technical";
    root.append(tech);
  }
  function renderPlanRecovery(c) {
    const section = el("section", undefined, "panel"),
      form = el("form");
    add(
      section,
      el(
        "h2",
        c.question ? "한 가지만 확인해 주세요" : "기준을 다시 만들 수 있습니다",
      ),
    );
    let answer;
    if (c.question) {
      add(section, el("p", c.question));
      answer = el("textarea");
      answer.required = true;
      answer.maxLength = 2000;
      answer.placeholder = "이 요청에서 반드시 지켜야 할 의미를 적어 주세요.";
      form.append(field("답변", answer));
    } else {
      section.append(
        el(
          "p",
          "연결 상태를 확인한 뒤 같은 요청에서 다시 시도할 수 있습니다.",
          "muted",
        ),
      );
    }
    const submit = el(
      "button",
      c.question ? "답변하고 기준 다시 만들기" : "기준 다시 만들기",
      "primary",
    );
    submit.type = "submit";
    form.append(submit);
    form.onsubmit = (event) => {
      event.preventDefault();
      guard(async () => {
        state.check = await api("/checks/" + c.id + "/plan", "POST", {
          row_version: c.row_version,
          answer: answer?.value || "",
        });
        state.last = "";
        detail();
        schedule();
      }, submit);
    };
    section.append(form);
    root.append(section);
  }
  function renderDraft(c, target) {
    const section = el("section", undefined, "panel");
    const matchesDemo =
      target.template?.criteria?.length &&
      c.draft?.criteria?.length === target.template.criteria.length &&
      c.draft.criteria.every((x) =>
        target.template.criteria.some(
          (y) =>
            x.observer === y.observer &&
            x.source_ref === y.source_ref &&
            JSON.stringify(x.expected) === JSON.stringify(y.expected),
        ),
      );
    if (c.draft?.criteria?.length)
      section.append(
        el(
          "p",
          matchesDemo
            ? "등록된 데모 기준을 제안했습니다. 내용을 확인한 뒤 실행해 주세요."
            : "요청을 바탕으로 만든 기준입니다. 내용을 확인한 뒤 실행해 주세요.",
          "muted",
        ),
      );
    add(
      section,
      add(
        el("div", undefined, "row between"),
        el("h2", "확인 기준"),
        el(
          "span",
          `필수 ${(c.draft?.criteria || []).filter((x) => x.required).length}개 · 최대 5개`,
          "muted",
        ),
      ),
    );
    const ul = el("ol", undefined, "criteria");
    for (const x of c.draft?.criteria || []) {
      const li = el("li", undefined, "criterion");
      add(
        li,
        add(
          el("div", undefined, "row"),
          el("h3", x.title),
          el("span", x.required ? "필수" : "참고", "badge"),
          x.proposed ? el("span", "제안", "badge") : null,
        ),
        el("p", "예상값: " + val(x.expected)),
        el(
          "p",
          (x.source === "request"
            ? "요청에서 가져옴: "
            : x.source === "definition"
              ? "등록된 정의: "
              : "제안: ") + (x.source_ref || "출처 미확인"),
          "muted",
        ),
      );
      ul.append(li);
    }
    if (!ul.children.length)
      ul.append(
        el(
          "p",
          "아직 승인할 기준이 없습니다. 기준 생성 상태를 확인해 주세요.",
          "empty",
        ),
      );
    section.append(ul);
    const actions = el("div", undefined, "actions");
    if (!c.draft?.criteria?.length) {
      if (c.suggestions?.criteria?.length)
        actions.append(
          btn("제안된 기준 검토", () => editContract(c.suggestions), true),
        );
      if (target.template?.criteria?.length)
        actions.append(
          btn("등록된 데모 기준 불러오기", () => editContract(target.template)),
        );
    }
    add(
      actions,
      btn("기준 수정", () => editContract()),
      btn("이 기준으로 실행", () => startAttempt(), true),
    );
    actions.lastChild.disabled = !c.draft?.criteria?.length;
    section.append(actions);
    root.append(section);
  }
  function renderAttempt(a) {
    const missing = a.contract.criteria.filter(
      (c) =>
        !a.results?.some(
          (r) => r.id === c.id && ["PASS", "FAIL"].includes(r.status),
        ),
    ).length;
    const s = el(
      "section",
      undefined,
      "panel result-panel " + (a.verdict || a.state || ""),
    );
    if (active(a)) {
      add(
        s,
        el("h2", labels[String(a.state).toLowerCase()] || "확인 중"),
        el("p", a.progress || "실행 상태를 기다리고 있습니다."),
        el(
          "p",
          `기준 ${(a.results || []).filter((r) => r.status === "PASS" || r.status === "FAIL").length} / ${a.contract.criteria.length}개 확인 완료`,
          "muted",
        ),
        btn(a.cancel_requested ? "취소 요청됨" : "실행 취소", async () => {
          await api("/attempts/" + a.id + "/cancel", "POST", {});
          await refreshDetail(true);
        }),
      );
      s.lastChild.disabled = !!a.cancel_requested;
    } else {
      add(
        s,
        el("h2", labels[a.verdict] || "확인 불가", "result-title"),
        el(
          "p",
          a.verdict === "PASS"
            ? "현재 배포에서 요구사항을 충족했습니다."
            : a.verdict === "FAIL"
              ? "예상과 다른 결과가 있습니다." +
                (missing ? " 확인하지 못한 기준도 함께 확인해 주세요." : "")
              : "판정에 필요한 근거가 부족합니다. 아래 사유를 확인해 주세요.",
        ),
      );
    }
    s.append(
      el(
        "p",
        `확인 완료 ${a.contract.criteria.length - missing}개 · 확인 불가 ${missing}개`,
        "muted",
      ),
    );
    const observations = (a.results || [])
      .flatMap((r) => r.evidence || [])
      .map((e) => e.at)
      .filter(Boolean)
      .sort();
    add(
      s,
      el(
        "p",
        `${a.target.environment || "환경 미확인"} · 관측 ${date(observations.at(-1))} · ${versionText(a)}`,
        "muted",
      ),
      el("p", comparisonText(a), "muted"),
    );
    if (a.action_journal?.length) {
      const journal = el("details");
      journal.dataset.key = "journal-" + a.id;
      journal.append(
        el(
          "summary",
          "실행 단계 " +
            a.action_journal.filter((e) => e.state === "DONE").length +
            "/" +
            a.action_journal.length,
        ),
      );
      const steps = el("ol", undefined, "journal");
      for (const event of a.action_journal)
        steps.append(
          el(
            "li",
            `${event.title} · ${{ RUNNING: "진행 중", DONE: "완료", FAILED: "중단" }[event.state] || event.state} · ${date(event.started_at)}`,
          ),
        );
      journal.append(steps);
      s.append(journal);
    }
    const ul = el("ul", undefined, "criteria");
    for (const x of a.contract.criteria || []) {
      const r = (a.results || []).find((r) => r.id === x.id),
        li = el("li", undefined, "criterion");
      if (r?.status) li.classList.add(r.status);
      add(
        li,
        add(
          el("div", undefined, "row"),
          el("h3", x.title),
          criterionBadge(r?.status || (active(a) ? "queued" : "UNKNOWN")),
        ),
        el("p", "예상값: " + val(x.expected)),
      );
      if (r?.reason) {
        li.append(el("p", reasonText(r.reason)));
        const raw = inspectDetails("상세 사유", r.reason);
        raw.dataset.key = "reason-" + a.id + "-" + x.id;
        li.append(raw);
      }
      if (r?.evidence?.length) {
        const d = el("details");
        d.dataset.key = "evidence-" + a.id + "-" + x.id;
        d.append(el("summary", "근거 " + r.evidence.length + "개 보기"));
        for (const e of r.evidence) {
          const box = el("div", undefined, "evidence"),
            link = el("a", "관측 근거 원문");
          link.href =
            "/api/proof/attempts/" +
            encodeURIComponent(a.id) +
            "/evidence/" +
            encodeURIComponent(e.id);
          link.target = "_blank";
          link.rel = "noopener";
          add(
            box,
            el("p", "실제값: " + val(e.actual)),
            el("p", `${sourceLabel(e.source)} · ${date(e.at)}`, "muted"),
          );
          if (
            [
              "missing",
              "expired",
              "evidence_missing",
              "evidence_expired",
            ].includes(e.availability)
          ) {
            box.append(
              el(
                "p",
                /expired/.test(e.availability)
                  ? "근거 보관 기간이 지났습니다. 다시 실행해 주세요."
                  : "근거 파일이 없습니다. 다시 실행해 주세요.",
                "notice",
              ),
            );
          } else {
            box.append(link);
            if (e.screenshot) {
              const shot = el("a", "화면 근거 보기");
              shot.href = link.href + "?format=image";
              shot.target = "_blank";
              shot.rel = "noopener";
              box.append(shot);
            }
            if (e.before_screenshot) {
              const before = el("a", "변경 전 화면 보기");
              before.href = link.href + "?format=before";
              before.target = "_blank";
              before.rel = "noopener";
              box.append(before);
            }
          }
          d.append(box);
        }
        li.append(d);
      }
      ul.append(li);
    }
    s.append(ul);
    if (!active(a)) {
      const actions = el("div", undefined, "actions");
      add(
        actions,
        btn("같은 기준으로 다시 실행", () => startAttempt(a), true),
        btn("기준 수정", () => editContract()),
        btn("결과 복사", () => copyResult(a)),
      );
      s.append(actions);
      s.append(renderDisposition(a));
    }
    root.append(s);
  }
  function renderDisposition(a) {
    const box = el("details");
    box.dataset.key = "disposition-" + a.id;
    box.append(
      el(
        "summary",
        a.disposition
          ? "내 판단: " +
              ({
                ACCEPTED: "확인 완료",
                DEFERRED: "보류",
                REJECTED: "반려",
                OPEN: "미정",
              }[a.disposition] || a.disposition)
          : "결과에 대한 내 판단 기록",
      ),
    );
    const form = el("form", undefined, "disposition-form"),
      select = el("select"),
      reason = el("input");
    for (const [value, text] of [
      ["ACCEPTED", "확인 완료"],
      ["DEFERRED", "보류"],
      ["REJECTED", "반려"],
    ]) {
      const option = el("option", text);
      option.value = value;
      select.append(option);
    }
    if (["ACCEPTED", "DEFERRED", "REJECTED"].includes(a.disposition))
      select.value = a.disposition;
    reason.maxLength = 500;
    reason.placeholder = "사유는 선택 사항입니다.";
    add(form, field("판단", select), field("사유", reason));
    const save = el("button", "판단 저장", "");
    save.type = "submit";
    form.append(save);
    form.onsubmit = (event) => {
      event.preventDefault();
      guard(async () => {
        await api("/attempts/" + a.id + "/disposition", "POST", {
          disposition: select.value,
          reason: reason.value,
        });
        await refreshDetail(true);
        message("내 판단을 기록했습니다. 기술 결과는 바뀌지 않습니다.");
      }, save);
    };
    box.append(form);
    const latest = a.disposition_history?.at(-1);
    if (latest)
      box.append(
        el(
          "p",
          `${latest.actor} · ${date(latest.at)}${latest.reason ? " · " + latest.reason : ""}`,
          "muted",
        ),
      );
    return box;
  }
  function renderHistory() {
    if (!state.attempts.length) return;
    const section = el("section", undefined, "section"),
      select = el("select");
    for (const a of state.attempts) {
      const o = el(
        "option",
        `${date(a.approved_at)} · ${labels[a.verdict] || labels[String(a.state).toLowerCase()] || a.state}`,
      );
      o.value = a.id;
      select.append(o);
    }
    select.value = selectedAttempt().id;
    select.onchange = () => {
      state.selected = select.value;
      state.viewDraft = false;
      detail();
    };
    add(section, field("이전 실행 보기", select));
    root.append(section);
  }
  async function startAttempt(source) {
    const c = state.check;
    if (source && source.approval.contract_hash !== c.contract_hash) {
      message(
        "이전 실행 이후 기준이 변경됐습니다. 현재 기준을 확인하고 명시적으로 실행해 주세요.",
      );
      state.selected = null;
      state.viewDraft = true;
      detail();
      return;
    }
    const currentRegistryHash = state.targets[c.target_ref]?.registry_hash;
    const registryHash = source
      ? source.plan?.registry_hash || source.approval?.registry_hash
      : currentRegistryHash;
    if (
      !currentRegistryHash ||
      (source && (!registryHash || registryHash !== currentRegistryHash))
    ) {
      message(
        "이전 실행의 확인 범위가 현재 설정과 다르거나 기록되지 않았습니다. 현재 계정과 허용된 동작을 확인한 뒤 다시 실행해 주세요.",
      );
      state.selected = null;
      state.viewDraft = true;
      detail();
      return;
    }
    const key =
      "proof-pending-" +
      c.id +
      "-" +
      c.contract_hash +
      "-" +
      registryHash +
      "-" +
      (source?.verdict === "FAIL" ? source.id : "current");
    let id;
    try {
      id = sessionStorage.getItem(key);
      if (!id) {
        id = crypto.randomUUID();
        sessionStorage.setItem(key, id);
      }
    } catch (e) {
      id = state.pending || (state.pending = crypto.randomUUID());
    }
    const a = await api("/checks/" + c.id + "/attempts", "POST", {
      revision: c.current_revision,
      contract_hash: c.contract_hash,
      scope: c.target_ref,
      registry_hash: registryHash,
      idempotency_key: id,
      baseline: source?.verdict === "FAIL" ? source.id : undefined,
    });
    try {
      sessionStorage.removeItem(key);
    } catch (e) {}
    state.pending = null;
    state.selected = a.id;
    state.viewDraft = false;
    state.editing = false;
    message("");
    await refreshDetail(true);
    schedule();
  }
  async function copyResult(a) {
    const lines = [
      state.check.request,
      labels[a.verdict] || "확인 불가",
      `환경: ${a.target.environment}; 계정: ${a.target.personas?.[a.contract.persona]?.account || a.contract.persona}`,
      `실행 ID: ${a.id}`,
      `완료 시각: ${date(a.finished_at)}; 배포: ${versionText(a)}`,
    ];
    for (const c of a.contract.criteria) {
      const r = a.results?.find((r) => r.id === c.id);
      lines.push(
        `${c.title}: ${{ PASS: "충족", FAIL: "불일치", UNKNOWN: "확인 불가" }[r?.status] || "확인 불가"}; 예상값 ${val(c.expected)}${r?.reason ? "; " + reasonText(r.reason) : ""}`,
      );
    }
    const exclusions = (state.check.revision_history || []).filter(
      (rev) =>
        rev.reason &&
        new Date(rev.at) <= new Date(a.approved_at) &&
        rev.contract?.criteria?.some(
          (c) => !a.contract.criteria.some((current) => current.id === c.id),
        ),
    );
    for (const rev of exclusions) lines.push("제외 사유: " + rev.reason);
    lines.push(
      "확인 범위에서 지원하지 않는 항목: " +
        (a.plan.unsupported?.join(", ") || "등록된 항목 없음"),
      comparisonText(a),
    );
    await navigator.clipboard.writeText(lines.join("\n"));
    message("결과를 복사했습니다.");
  }
  function editContract(seed) {
    state.editing = true;
    const c = state.check,
      draft = structuredClone(seed || c.draft);
    clear();
    add(
      root,
      el("h1", "확인 기준 수정"),
      el(
        "p",
        "저장하면 새 기준 버전이 만들어집니다. 실행 전에 변경한 내용을 다시 확인합니다.",
        "muted",
      ),
    );
    const form = el("form", undefined, "panel"),
      ul = el("div");
    let removed = [];
    for (const x of draft.criteria || []) {
      const row = el("div", undefined, "criterion"),
        title = el("input");
      title.value = x.title;
      title.required = true;
      const type = el("select");
      for (const [k, v] of [
        ["string", "문자"],
        ["number", "숫자"],
        ["boolean", "참 / 거짓"],
        ["null", "null"],
        ["missing", "값 없음"],
        ["json", "객체 / 배열"],
      ]) {
        const o = el("option", v);
        o.value = k;
        type.append(o);
      }
      type.value = !x.expected.present
        ? "missing"
        : x.expected.data === null
          ? "null"
          : ["string", "number", "boolean"].includes(typeof x.expected.data)
            ? typeof x.expected.data
            : "json";
      const input = el("input");
      input.value =
        typeof x.expected.data === "object"
          ? JSON.stringify(x.expected.data)
          : String(x.expected.data ?? "");
      const boolean = el("select");
      for (const [v, t] of [
        ["true", "참"],
        ["false", "거짓"],
      ]) {
        const o = el("option", t);
        o.value = v;
        boolean.append(o);
      }
      boolean.value = String(x.expected.data === true);
      const sync = () => {
        boolean.hidden = type.value !== "boolean";
        input.hidden = type.value === "boolean";
        input.disabled = ["null", "missing"].includes(type.value);
        input.type = type.value === "number" ? "number" : "text";
        input.step = "any";
      };
      type.onchange = sync;
      sync();
      add(
        row,
        field("기준", title),
        add(
          el("div", undefined, "pair"),
          field("예상값 유형", type),
          field("예상값", add(el("span"), input, boolean)),
        ),
        el(
          "p",
          (x.required ? "필수 기준 · " : "참고 기준 · ") +
            (x.source === "request" ? "요청: " : "등록된 정의: ") +
            x.source_ref,
          "muted",
        ),
      );
      const source = el("select");
      for (const [v, t] of [
        ["request", "요청 원문"],
        ["definition", "등록된 정의"],
      ]) {
        const o = el("option", t);
        o.value = v;
        source.append(o);
      }
      source.value = x.source;
      const ref = el("input");
      ref.value = x.source_ref || "";
      ref.required = true;
      add(
        row,
        add(
          el("div", undefined, "pair"),
          field("예상값의 출처", source),
          field("요청 원문의 정확한 인용 또는 정의 키", ref),
        ),
      );
      const remove = btn("이 기준 제외", () => {
        const box = el("div", undefined, "removal"),
          reason = el("input");
        reason.placeholder = "제외하는 이유";
        reason.required = true;
        add(
          box,
          field("제외 사유", reason),
          btn("제외 확정", () => {
            if (!reason.reportValidity()) return;
            removed.push({ id: x.id, reason: reason.value });
            row.remove();
          }),
        );
        row.append(box);
        remove.disabled = true;
      });
      row.append(remove);
      row.read = () => {
        let data = input.value;
        switch (type.value) {
          case "number":
            if (!input.value.trim() || !Number.isFinite(Number(input.value)))
              throw Error("예상값에 올바른 숫자를 입력해 주세요.");
            data = Number(input.value);
            break;
          case "boolean":
            data = boolean.value === "true";
            break;
          case "null":
          case "missing":
            data = null;
            break;
          case "json":
            try {
              data = JSON.parse(input.value);
            } catch (e) {
              throw Error("객체 또는 배열의 JSON 형식을 확인해 주세요.");
            }
        }
        return {
          ...x,
          title: title.value,
          source: source.value,
          source_ref: ref.value,
          expected: { present: type.value !== "missing", data },
        };
      };
      ul.append(row);
    }
    const save = el("button", "변경한 기준 저장", "primary");
    save.type = "submit";
    add(
      form,
      ul,
      add(
        el("div", undefined, "actions"),
        save,
        btn("수정 취소", () => {
          state.editing = false;
          detail();
        }),
      ),
    );
    form.onsubmit = (e) => {
      e.preventDefault();
      guard(async () => {
        draft.criteria = [...ul.children].map((row) => row.read());
        const required = draft.criteria.filter((x) => x.required).length;
        if (required < 1 || required > 5)
          throw Error("필수 기준은 1개 이상, 5개 이하로 유지해 주세요.");
        state.check = await api("/checks/" + c.id + "/contract", "PATCH", {
          row_version: c.row_version,
          contract: draft,
          removal_reason: removed.map((r) => r.id + ": " + r.reason).join("; "),
        });
        state.editing = false;
        state.selected = null;
        state.viewDraft = true;
        state.last = "";
        detail();
        message("기준을 저장했습니다. 변경한 기준으로 실행할 수 있습니다.");
      }, save);
    };
    root.append(form);
    root.querySelector("input")?.focus();
  }
  async function refreshDetail(force = false) {
    const id = decodeURIComponent(location.pathname.split("/")[2]);
    const d = await api("/checks/" + encodeURIComponent(id));
    state.check = d.check;
    state.attempts = (d.attempts || []).sort(
      (a, b) => new Date(b.approved_at) - new Date(a.approved_at),
    );
    const sig = JSON.stringify(d);
    if (!state.editing && !state.busy && (force || sig !== state.last)) {
      state.last = sig;
      preserveRender(detail);
    } else if (force && !state.editing) {
      state.last = sig;
      preserveRender(detail);
    }
  }
  function schedule() {
    clearTimeout(state.timer);
    if (document.hidden) return;
    const fast =
      active(selectedAttempt()) ||
      ["queued", "pending", "planning", "running"].includes(
        String(state.check?.planning).toLowerCase(),
      );
    state.timer = setTimeout(
      async () => {
        try {
          if (location.pathname.startsWith("/checks/")) await refreshDetail();
          else await refreshHome();
        } catch (e) {
          showError(e);
        } finally {
          schedule();
        }
      },
      fast ? 2000 : 30000,
    );
  }
  document.addEventListener("visibilitychange", () => {
    if (!document.hidden) {
      if (location.pathname.startsWith("/checks/"))
        refreshDetail().catch(showError);
      else refreshHome();
    }
    schedule();
  });
  function login() {
    clear();
    message("");
    add(
      root,
      el("h1", "ProofQA에 로그인"),
      el(
        "p",
        "한 계정으로 요청 작성부터 결과 판단까지 이어서 확인합니다.",
        "intro muted",
      ),
    );
    const form = el("form", undefined, "panel"),
      name = el("input"),
      password = el("input");
    name.required = true;
    name.autocomplete = "username";
    password.required = true;
    password.type = "password";
    password.autocomplete = "current-password";
    const submit = el("button", "로그인", "primary");
    submit.type = "submit";
    add(form, field("계정", name), field("비밀번호", password), submit);
    form.onsubmit = (event) => {
      event.preventDefault();
      guard(async () => {
        await api("/login", "POST", {
          name: name.value,
          password: password.value,
        });
        password.value = "";
        await boot();
      }, submit);
    };
    root.append(form);
    name.focus();
  }
  async function boot() {
    try {
      const [session, registry] = await Promise.all([
        api("/session"),
        api("/registry"),
      ]);
      state.session = session;
      state.csrf = session.csrf;
      state.targets = registry.targets || {};
      message("");
      if (location.pathname.startsWith("/checks/")) await refreshDetail(true);
      else home();
      schedule();
    } catch (e) {
      if (e.status === 401) {
        login();
        return;
      }
      message(e.message);
      root.replaceChildren(btn("다시 불러오기", boot));
    }
  }
  boot();
})();
