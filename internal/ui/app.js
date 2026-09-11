// app.js: what every dashboard page shares. Loaded before each page's own
// script. Keeps the vocabulary (Korean labels for machine states) in one
// place so the three pages never disagree about what "APP_FAILURE" means.
'use strict';

const $ = (id) => document.getElementById(id);
const esc = (s) => String(s ?? '').replace(/[&<>"]/g, c => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;' }[c]));

// ---- preferences (density, developer info, theme) ---------------------------
const prefs = {
  dev: localStorage.getItem('vigil.dev') === '1',
  density: localStorage.getItem('vigil.density') || 'comfortable',
  theme: localStorage.getItem('vigil.theme') || 'auto',
};
function applyPrefs() {
  document.body.classList.toggle('devmode', prefs.dev);
  document.body.classList.toggle('compact', prefs.density === 'compact');
  document.documentElement.dataset.theme = prefs.theme;
  // 개발자 정보 opens the technical layer of every cause block already on screen.
  if (prefs.dev) document.querySelectorAll('details.cause-l3').forEach(d => { d.open = true; });
}
applyPrefs();
// The rail controls are optional (the report page has none).
if ($('devmode')) { $('devmode').checked = prefs.dev; $('devmode').onchange = (e) => { prefs.dev = e.target.checked; localStorage.setItem('vigil.dev', prefs.dev ? '1' : '0'); applyPrefs(); }; }
if ($('density')) { $('density').value = prefs.density; $('density').onchange = (e) => { prefs.density = e.target.value; localStorage.setItem('vigil.density', prefs.density); applyPrefs(); }; }
if ($('theme')) { $('theme').value = prefs.theme; $('theme').onchange = (e) => { prefs.theme = e.target.value; localStorage.setItem('vigil.theme', prefs.theme); applyPrefs(); }; }

// ---- vocabulary (plain Korean, one register) --------------------------------
const OUTCOME = {
  PASS: ['정상', 'ok'], APP_FAILURE: ['서비스 문제', 'bad'], QA_FLAKE: ['일시 오류, 재검사 정상', 'warn'], SCRIPT_DRIFT: ['화면이 바뀜, 검사 수정 필요', 'warn'],
  LIGHTPANDA_INCOMPATIBLE: ['다른 브라우저로 재확인', 'info'], BROWSER_AMBIGUOUS: ['재확인 필요', 'info'], AUTH_FAILURE: ['로그인 문제', 'warn'], DATA_FAILURE: ['데이터 문제', 'warn'],
  ENV_FAILURE: ['검사 환경 문제', 'warn'], DEPLOYMENT_NOT_READY: ['배포 대기', 'muted'], ORACLE_UNKNOWN: ['판단 기준 불명확', 'warn'], NEEDS_REVIEW: ['사람 확인 필요', 'warn'],
};
const STATE = {
  ACTIVE: ['정식 검사', 'ok'], SOAK: ['안정화 중', 'info'], CANDIDATE: ['후보', 'muted'], NEEDS_REVIEW: ['사람 확인 필요', 'warn'], PENDING_APPROVAL: ['승인 대기', 'warn'], QUARANTINED: ['불안정, 잠시 제외', 'warn'],
  DUPLICATE: ['중복', 'muted'], EPHEMERAL: ['일회성', 'muted'], REJECTED: ['제외', 'muted'], MERGED: ['통합', 'muted'], SUPERSEDED: ['대체됨', 'muted'], RETIRED: ['은퇴', 'muted'],
};
const READY = { READY: ['배포 확인', 'ok'], WAITING_FOR_DEPLOYMENT: ['배포 기다리는 중', 'muted'], DEPLOYMENT_UNKNOWN: ['확인 못 함, 시간 경과로 진행', 'warn'] };
const KIND = { RUN_SCENARIO: '검사 실행', AGENT_DISCOVER: 'AI가 새 기능 살펴보기', AGENT_VERIFY: 'AI가 변경 사항 확인', AGENT_REPAIR: 'AI가 검사 스크립트 수리', CHROMIUM_CONFIRM: 'Chromium으로 재확인', VALIDATE_CANDIDATE: '새 검사 후보 검증' };
const CLASS = { P0: '매우 중요', P1: '중요', P2: '보통' };
const BROWSER = { lightpanda: '가벼운 브라우저', chromium: 'Chromium' };
const STEP = {
  goto: '페이지 이동', click: '클릭', fill: '입력', type: '입력', press: '키 입력', select: '선택', hover: '마우스 올리기', wait_for: '화면 표시 기다림', wait_ms: '잠시 기다림', wait_url: '주소 변경 기다림',
  assert_text: '문구 확인', assert_no_text: '문구 없음 확인', assert_visible: '표시 확인', assert_not_visible: '숨김 확인', assert_url: '주소 확인', assert_count: '개수 확인', assert_request: '서버 응답 확인', assert_attr: '상태 확인', expect_popup: '새 탭 열림 확인', eval: '값 확인', use_flow: '공통 절차', screenshot: '화면 캡처',
};
const DECISION = { NEW_SCRIPT: ['새 검사 항목 제안', 'ok'], PATCH_SCRIPT: ['검사 수리안 제안', 'ok'], APP_FAILURE: ['서비스 문제 발견', 'bad'], NO_NEW_COVERAGE: ['추가할 검사 없음', 'muted'], NEEDS_REVIEW: ['사람 확인 필요', 'warn'], ORACLE_UNKNOWN: ['판단 기준 불명확', 'warn'] };
const INCIDENT = { ENVIRONMENT: ['검사 환경 문제', 'warn'], APP_REGRESSION: ['서비스 결함 의심', 'bad'] };
// The kind chip already says what this is, so the stored title's technical prefix is noise.
const incTitle = t => String(t || '').replace(/^(APP_REGRESSION|ENVIRONMENT)\s*:\s*/, '');
const ORIGIN = { seed: ['시드', 'muted'], agent: ['AI 생성', 'info'], import: ['가져옴', 'muted'], human: ['사람', 'muted'] };
const WHO = { seed: '사람(파일)', agent: 'AI 도우미', repair: 'AI 수리', system: '시스템', human: '사람' };
const ORACLE = { spec: '명세', approved_qa: '승인된 검사', contract: '코드 계약', observation: '관찰만' };
const LINK = { feature: '기능', capability: '역량', route: '경로', api: 'API', path: '소스 파일', persona: '계정 역할' };
const REQ_STATUS = { queued: ['접수됨', 'info'], preempting: ['기존 작업 정리 중', 'info'], running: ['진행 중', 'ok'], budget_waiting: ['예산 대기', 'warn'], done: ['완료', 'muted'], failed: ['실패', 'bad'], stale: ['응답 없음', 'warn'], none: ['미실행', 'muted'] };

// status chip: shape + label, never colour alone
// environment badge (환경: prod); empty when the run predates environments
const envBadge = (env) => env ? `<span class="st st-muted env"><i></i>환경: ${esc(env)}</span>` : '';
const st = (map, v, fallback) => { const m = map[v]; if (!v && !fallback) return ''; return `<span class="st st-${m ? m[1] : 'muted'}"><i></i>${esc(m ? m[0] : (v || fallback))}</span>`; };
const dev = (s) => `<span class="dev mono">${esc(s)}</span>`;

// ---- time ------------------------------------------------------------------
// The server emits "YYYY-MM-DD HH:MM:SS" in its own local zone; the dashboard
// is an intranet tool that sits in the same zone.
const T = (s) => s ? new Date(s.replace(' ', 'T')) : null;
function ago(s) { const d = T(s); if (!d) return ''; const sec = (Date.now() - d) / 1000; if (sec < 60) return '방금'; if (sec < 3600) return `${Math.floor(sec / 60)}분 전`; if (sec < 86400) return `${Math.floor(sec / 3600)}시간 전`; return `${Math.floor(sec / 86400)}일 전`; }
function until(s) { const d = T(s); if (!d) return '곧'; const sec = (d - Date.now()) / 1000; if (sec <= 0) return '곧'; if (sec < 3600) return `${Math.ceil(sec / 60)}분 후`; return `${Math.floor(sec / 3600)}시간 ${Math.ceil((sec % 3600) / 60)}분 후`; }
const hm = (s) => s ? s.slice(11, 16) : '';
const ymd = (s) => s ? s.slice(0, 10) : '';
// clock, plus the date when it is not today (a bare "10:53" from last Tuesday misleads)
function when(s) { if (!s) return ''; const today = new Date().toISOString().slice(0, 10) === ymd(s) || new Date().toLocaleDateString('sv-SE') === ymd(s); return today ? hm(s) : `${ymd(s).slice(5).replace('-', '/')} ${hm(s)}`; }
const secs = (ms) => (ms / 1000).toFixed(1) + '초';
const flowOf = (n) => { const m = /^flow:([^/]+)\//.exec(n || ''); return m ? m[1] : ''; };

// ---- toast -----------------------------------------------------------------
function toast(msg) { const t = $('toast'); if (!t) return; t.textContent = msg; t.classList.add('show'); clearTimeout(t._h); t._h = setTimeout(() => t.classList.remove('show'), 3000); }

// ---- polling with change detection ------------------------------------------
// The server tags polled payloads with an ETag that ignores the wall clock.
// When the tag repeats, the page skips its re-render, so an open <details>,
// a text selection or a scrolled list is not wiped every few seconds.
function poller({ url, every, onData, onError, tick }) {
  let etag = null, lastOk = 0, timer = null, inflight = false;
  async function refresh(force) {
    if (inflight) return;
    inflight = true;
    try {
      const r = await fetch(typeof url === 'function' ? url() : url, { cache: 'no-cache' });
      if (!r.ok) throw new Error('HTTP ' + r.status);
      const tag = r.headers.get('ETag');
      const changed = force || !tag || tag !== etag;
      etag = tag;
      const data = await r.json();
      lastOk = Date.now();
      if ($('live')) $('live').classList.remove('off');
      if ($('freshText')) $('freshText').textContent = '실시간 갱신 ' + new Date(data.now || lastOk).toLocaleTimeString('ko-KR', { hour: '2-digit', minute: '2-digit', second: '2-digit' });
      if ($('fetchErr')) $('fetchErr').innerHTML = '';
      onData(data, changed);
    } catch (e) {
      if ($('live')) $('live').classList.add('off');
      if ($('freshText')) $('freshText').textContent = lastOk ? `갱신 실패, 마지막 성공 ${ago(new Date(lastOk).toLocaleString('sv-SE'))}` : '연결 실패';
      if (onError) onError(e);
      else if ($('fetchErr')) $('fetchErr').innerHTML = `<div class="errbox"><span>현황 서버에 연결하지 못했습니다 (${esc(e.message)}). vigil가 실행 중인지 확인해 주세요.</span><button class="btn" id="retryBtn">다시 시도</button></div>`;
      const rb = $('retryBtn'); if (rb) rb.onclick = () => refresh(true);
    } finally { inflight = false; }
  }
  function start() {
    refresh(true);
    timer = setInterval(() => { if (!document.hidden) refresh(false); }, every);
    // relative times ("4분 전") must move even when the data does not
    if (tick) setInterval(() => { if (!document.hidden) (tick.fn || tick)(); }, tick.every || 30000);
    // a hidden tab stops polling; catch up the moment it is shown again
    document.addEventListener('visibilitychange', () => { if (!document.hidden) refresh(false); });
  }
  return { start, refresh: () => refresh(true), stop: () => clearInterval(timer) };
}

// ---- keyboard-reachable rows -------------------------------------------------
// Clickable table rows get a tab stop and respond to Enter/Space, so the
// dashboard works without a mouse and screen readers announce them as buttons.
function bindRows(selector, handler) {
  document.querySelectorAll(selector).forEach(tr => {
    tr.tabIndex = 0; tr.setAttribute('role', 'button');
    tr.onclick = () => handler(tr);
    tr.onkeydown = (e) => { if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); handler(tr); } };
  });
}

// ---- <details> state that survives re-render -------------------------------
// Pages rebuild panels with innerHTML; remember which <details data-key> were
// open and reopen them afterwards.
const openDetails = new Set();
function keepDetails(root) {
  (root || document).querySelectorAll('details[data-key]').forEach(d => {
    if (openDetails.has(d.dataset.key)) d.open = true;
    d.addEventListener('toggle', () => { d.open ? openDetails.add(d.dataset.key) : openDetails.delete(d.dataset.key); });
  });
}

// ---- brand -------------------------------------------------------------------
function setTarget(target) { if ($('brandTarget') && target) $('brandTarget').textContent = target.replace(/^https?:\/\//, ''); }

// ---- run steps (shared by activity, verification, report) --------------------
function stepsHTML(steps) {
  return `<div class="steps">${(steps || []).map(x => {
    const flow = flowOf(x.name);
    const detail = x.ok ? (x.expected || x.actual || '') : [x.expected ? `기대: ${x.expected}` : '', x.actual ? `실제: ${x.actual}` : '', x.error || ''].filter(Boolean).join(' / ');
    return `<div class="step"><span class="n">${x.index}</span>${st({ ok: ['성공', 'ok'], fail: ['실패', 'bad'] }, x.ok ? 'ok' : 'fail')}<span><b>${esc(STEP[x.kind] || x.kind)}</b>${flow ? ` <span class="sub">공통 절차 ${esc(flow)}</span>` : ''} ${dev(x.name || x.kind)}<div class="how">${esc(detail)}</div></span></div>`;
  }).join('') || '<div class="empty">기록된 단계가 없습니다.</div>'}</div>`;
}
// ---- why a run did not pass: one sentence, then two deeper layers -----------
// The sentence is written once on the server (internal/explain) and arrives as
// row.cause = {headline, kind, why, detail, next}; the pages only decide which
// layer is visible. Level 1 is the sentence, level 2 (자세히) explains it in
// words, level 3 (기술 정보) holds the raw text and opens by itself in
// 개발자 정보 mode.
const CAUSE_KIND = {
  service: ['서비스 문제', 'bad'], script: ['검사 절차 문제', 'warn'], environment: ['검사 환경 문제', 'warn'],
  data: ['데이터 문제', 'warn'], browser: ['브라우저 재확인', 'info'], deployment: ['배포 대기', 'muted'], unknown: ['판단 근거 없음', 'warn'],
};
function causeHTML(row, key, extra) {
  const c = row && row.cause; if (!c) return '';
  const step = row.failed_step ? `<p class="sub">멈춘 곳: ${row.failed_step}단계 ${esc(STEP[row.failed_action] || row.failed_action || '')}</p>` : '';
  const l3 = c.detail ? `<details class="cause-l3" data-key="tech:${esc(key)}"${prefs.dev ? ' open' : ''}><summary>기술 정보</summary><pre class="raw">${esc(c.detail)}</pre></details>` : '';
  return `<div class="cause">
    <p class="cause-l1">${st(CAUSE_KIND, c.kind)}<span class="cause-line">${esc(c.headline)}</span></p>
    <details class="cause-l2" data-key="cause:${esc(key)}"><summary>자세히</summary><div class="cause-body">${c.why ? `<p>${esc(c.why)}</p>` : ''}${step}${extra || ''}${c.next ? `<p class="cause-next">${esc(c.next)}</p>` : ''}${l3}</div></details>
  </div>`;
}
// A cause block may sit inside a clickable row; its toggles must not also open
// the row. Remembers open layers across re-renders like every other <details>.
function bindCause(root) {
  (root || document).querySelectorAll('.cause summary').forEach(s => s.addEventListener('click', (e) => e.stopPropagation()));
  keepDetails(root);
}

// ---- plain-language glossary ------------------------------------------------
// Every internal word the pages show gets one sentence of "what this means for
// you". /help prints the same sentences in a table, so the two never drift.
const GLOSS = {
  ACTIVE: '사람이 승인한 검사입니다. 매일 정해진 시각과 배포 직후에 자동으로 실행됩니다.',
  SOAK: '아직 믿을 수 있는지 지켜보는 중입니다. 연속으로 통과하면 정식 검사가 됩니다.',
  PENDING_APPROVAL: 'AI가 만들고 실행까지 마쳤습니다. 사람이 승인해야 매일 실행됩니다.',
  CANDIDATE: 'AI가 방금 제안한 검사입니다. 자동 검증을 기다립니다.',
  NEEDS_REVIEW: 'AI가 스스로 고치지 못했습니다. 사람이 보고 승인하거나 반려해야 합니다.',
  QUARANTINED: '결과가 들쭉날쭉해서 자동 실행에서 잠시 빠졌습니다.',
  DUPLICATE: '같은 내용의 검사가 이미 있어 쓰지 않습니다.',
  EPHEMERAL: '한 번만 쓰고 버리는 검사입니다.',
  REJECTED: '사람이 반려해 실행하지 않습니다.',
  MERGED: '다른 검사에 합쳐졌습니다.',
  SUPERSEDED: '새 버전으로 대체됐습니다.',
  RETIRED: '더 쓰지 않는 검사입니다.',
  PASS: '약속대로 동작했습니다. 할 일이 없습니다.',
  APP_FAILURE: '서비스가 약속과 다르게 동작했습니다. 개발팀 확인이 필요합니다.',
  QA_FLAKE: '한 번 실패했지만 다시 하니 통과했습니다. 대개 일시적인 문제입니다.',
  SCRIPT_DRIFT: '화면 구조가 바뀌어 검사 절차를 고쳐야 합니다.',
  LIGHTPANDA_INCOMPATIBLE: '가벼운 브라우저로는 판단할 수 없어 Chromium으로 다시 확인합니다.',
  BROWSER_AMBIGUOUS: '브라우저마다 결과가 달라 사람이 확인해야 합니다.',
  AUTH_FAILURE: '로그인에 실패했습니다. 계정이나 토큰을 확인해 주세요.',
  DATA_FAILURE: '검사에 필요한 데이터가 없거나 달라졌습니다.',
  ENV_FAILURE: '서비스가 아니라 검사 환경(네트워크, 접속) 문제입니다.',
  DEPLOYMENT_NOT_READY: '새 배포가 아직 올라오지 않아 기다리는 중입니다.',
  ORACLE_UNKNOWN: '무엇이 옳은 동작인지 근거가 없어 판단하지 못했습니다.',
  spec: '기획 문서나 이슈에 적힌 약속을 기준으로 판단합니다.',
  contract: '코드에 적힌 규칙(API 응답, 화면 계약)을 기준으로 판단합니다.',
  approved_qa: '사람이 승인한 검사 절차 자체를 기준으로 판단합니다.',
  observation: '지금 동작을 그대로 기준으로 삼습니다. 실패해도 결함이라 단정할 수 없습니다.',
  confirmed: '이슈에 적힌 증상이 실제로 다시 나타났습니다.',
  disputed: '실행 결과와 AI의 설명이 어긋납니다. 사람이 증거를 보고 판단해 주세요.',
  unconfirmed: '증상이 다시 나타나지 않았습니다.',
  marker: '배포된 파일에 붙은 버전 표시입니다. 어느 배포에서 검사했는지 구분합니다.',
  finding: '화면 값과 서버 응답, 도메인 규칙을 대조해 찾은 이상한 점입니다.',
};
// tip renders a term with its plain-language sentence: visible label, tooltip on
// hover, and the same sentence announced to screen readers.
const tip = (label, explain) => explain
  ? `<span class="tip" tabindex="0" title="${esc(explain)}" aria-label="${esc(label)}. ${esc(explain)}">${esc(label)}</span>`
  : esc(label);
// stTip is a status chip that also carries its glossary sentence.
const stTip = (map, v) => {
  const m = map[v], g = GLOSS[v];
  if (!v) return '';
  const label = m ? m[0] : v;
  return `<span class="st st-${m ? m[1] : 'muted'}"${g ? ` title="${esc(g)}" aria-label="${esc(label)}. ${esc(g)}"` : ''}><i></i>${esc(label)}</span>`;
};

// ---- "what happens next" for a script state --------------------------------
function nextStep(s, dailyAt) {
  const at = dailyAt || '09:00';
  switch (s.state) {
    case 'PENDING_APPROVAL': return `승인하면 매일 ${at}에 자동 실행됩니다.`;
    case 'SOAK': return `${s.soak_target || 3}회 연속 통과하면 정식 검사가 됩니다. 지금 ${s.soak_passes || 0}회.`;
    case 'ACTIVE': return `매일 ${at}과 배포 직후에 자동 실행됩니다.`;
    case 'NEEDS_REVIEW': return '사람이 승인하면 안정화 단계부터 다시 시작합니다.';
    case 'QUARANTINED': return '자동 실행에서 빠져 있습니다. 고친 뒤 다시 승인해 주세요.';
    case 'CANDIDATE': return '자동 검증을 통과하면 안정화 단계로 넘어갑니다.';
    default: return '자동 실행하지 않습니다.';
  }
}
// where a script came from, in one chip
function sourceChip(s) {
  if (s.source_kind === 'issue' && s.source_ref) return `<span class="st st-info"><i></i>${esc(s.source_ref)}</span>`;
  if (s.source_kind === 'log') return '<span class="st st-info"><i></i>운영 로그</span>';
  if (s.source_kind === 'exec') return '<span class="st st-info"><i></i>직접 요청</span>';
  return s.origin === 'agent' ? '<span class="st st-info"><i></i>AI 생성</span>' : '<span class="st st-muted"><i></i>사람 작성</span>';
}

// ---- rail counts -------------------------------------------------------------
// A count next to a rail link. Announced politely so a screen reader hears the
// number change without losing the user's place.
function railCount(id, n) {
  const el = $(id); if (!el) return;
  el.textContent = n > 0 ? n : '';
}

// ---- collapsible group state (survives re-render) ----------------------------
// Groups remember open/closed per page in localStorage, keyed by name.
function groupOpen(page, key, fallback) {
  const v = localStorage.getItem(`vigil.group.${page}.${key}`);
  return v === null ? fallback : v === '1';
}
function setGroupOpen(page, key, open) { localStorage.setItem(`vigil.group.${page}.${key}`, open ? '1' : '0'); }
