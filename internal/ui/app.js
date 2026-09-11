// app.js: what every dashboard page shares. Loaded before each page's own
// script. Keeps the vocabulary (Korean labels for machine states) in one
// place so the three pages never disagree about what "APP_FAILURE" means.
'use strict';

const $ = (id) => document.getElementById(id);
const esc = (s) => String(s ?? '').replace(/[&<>"]/g, c => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;' }[c]));

// ---- 저장소 래퍼 ------------------------------------------------------------
// 사내 정책이나 쿠키 차단 설정에서는 localStorage 접근 자체가 예외를 던집니다.
// 최상위에서 던지면 app.js 가 그 줄에서 멈춰 모든 화면이 백지가 되므로, 읽기는
// null, 쓰기는 무시로 떨어뜨려 기본값으로 동작하게 합니다. 키 이름은 그대로입니다.
const store = {
  get(k) { try { return localStorage.getItem(k); } catch { return null; } },
  set(k, v) { try { localStorage.setItem(k, v); } catch { /* 저장소가 막혀 있으면 이번 세션에만 적용됩니다. */ } },
  // '1'/'0' 으로 저장한 켜짐 여부. 예전에 쓰던 'open'/'closed' 표기도 읽어 줍니다.
  on(k) { const v = this.get(k); return v === '1' || v === 'open'; },
  setOn(k, v) { this.set(k, v ? '1' : '0'); },
};

// ---- preferences (density, developer info, theme) ---------------------------
const prefs = {
  dev: store.get('vigil.dev') === '1',
  density: store.get('vigil.density') || 'comfortable',
  theme: store.get('vigil.theme') || 'auto',
};
// 'auto' 는 저장값으로만 남기고, 화면에는 실제로 계산된 밝게/어둡게를 적용합니다.
// 그래야 theme.css 가 다크 토큰을 한 곳에만 둘 수 있습니다.
const darkQuery = window.matchMedia('(prefers-color-scheme: dark)');
const resolveTheme = (t) => (t === 'auto' ? (darkQuery.matches ? 'dark' : 'light') : t);
function applyPrefs() {
  document.body.classList.toggle('devmode', prefs.dev);
  // 밀도는 html 에 붙여야 html { font-size:var(--fs) } 가 낮춘 값을 실제로 읽습니다.
  document.documentElement.classList.toggle('compact', prefs.density === 'compact');
  document.documentElement.dataset.theme = resolveTheme(prefs.theme);
  // 개발자 정보 opens the technical layer of every cause block already on screen.
  if (prefs.dev) document.querySelectorAll('details.cause-l3').forEach(d => { d.open = true; });
}
applyPrefs();
// 시스템 설정을 따르는 동안 사용자가 밝게/어둡게를 바꾸면 즉시 반영합니다.
darkQuery.addEventListener('change', () => { if (prefs.theme === 'auto') applyPrefs(); });
// The rail controls are optional (the report page has none).
if ($('devmode')) { $('devmode').checked = prefs.dev; $('devmode').onchange = (e) => { prefs.dev = e.target.checked; store.setOn('vigil.dev', prefs.dev); applyPrefs(); }; }
if ($('density')) { $('density').value = prefs.density; $('density').onchange = (e) => { prefs.density = e.target.value; store.set('vigil.density', prefs.density); applyPrefs(); }; }
if ($('theme')) { $('theme').value = prefs.theme; $('theme').onchange = (e) => { prefs.theme = e.target.value; store.set('vigil.theme', prefs.theme); applyPrefs(); }; }

// 좁은 화면에서는 설명과 설정을 접어 두어 첫 화면이 현재 상태로 시작하게 합니다.
// 넓은 화면과 스크립트가 없는 환경에서는 HTML 의 open 속성 그대로 펼쳐져 있습니다.
const narrowQuery = window.matchMedia('(max-width:820px)');
function foldOnNarrow(el) {
  if (!el) return;
  const sync = () => { el.open = !narrowQuery.matches; };
  sync();
  narrowQuery.addEventListener('change', sync);
}
foldOnNarrow($('railPrefs'));

// ---- vocabulary (plain Korean, one register) --------------------------------
const OUTCOME = {
  PASS: ['정상', 'ok'], APP_FAILURE: ['서비스 문제', 'bad'], QA_FLAKE: ['다시 하니 통과', 'warn'], SCRIPT_DRIFT: ['화면이 바뀜, 검사 수정 필요', 'warn'],
  LIGHTPANDA_INCOMPATIBLE: ['다른 브라우저로 재확인', 'info'], BROWSER_AMBIGUOUS: ['재확인 필요', 'info'], AUTH_FAILURE: ['로그인 문제', 'warn'], DATA_FAILURE: ['데이터 문제', 'warn'],
  ENV_FAILURE: ['검사 환경 문제', 'warn'], DEPLOYMENT_NOT_READY: ['배포 대기', 'muted'], ORACLE_UNKNOWN: ['판단 근거 없음', 'warn'], NEEDS_REVIEW: ['사람 확인 필요', 'warn'],
};
const STATE = {
  ACTIVE: ['정식 검사', 'ok'], SOAK: ['안정화 중', 'info'], CANDIDATE: ['후보', 'muted'], NEEDS_REVIEW: ['사람 확인 필요', 'warn'], PENDING_APPROVAL: ['승인 대기', 'warn'], QUARANTINED: ['불안정, 잠시 제외', 'warn'],
  DUPLICATE: ['중복', 'muted'], EPHEMERAL: ['일회성', 'muted'], REJECTED: ['제외', 'muted'], MERGED: ['통합', 'muted'], SUPERSEDED: ['대체됨', 'muted'], RETIRED: ['더 쓰지 않음', 'muted'],
};
const READY = { READY: ['배포 확인', 'ok'], WAITING_FOR_DEPLOYMENT: ['배포 기다리는 중', 'muted'], DEPLOYMENT_UNKNOWN: ['확인 못 함, 시간 경과로 진행', 'warn'] };
const KIND = { RUN_SCENARIO: '검사 실행', AGENT_DISCOVER: 'AI가 새 기능 살펴보기', AGENT_VERIFY: 'AI가 변경 사항 확인', AGENT_REPAIR: 'AI가 검사 스크립트 수리', CHROMIUM_CONFIRM: 'Chromium으로 재확인', VALIDATE_CANDIDATE: '새 검사 후보 검증' };
// 커버리지 확장이 지시하는 작업 이름. KIND 와 뜻이 겹치는 네 가지는 글자까지 같게
// 두어, 시스템 활동 화면의 두 표가 같은 작업을 다른 말로 부르지 않게 합니다.
const ACTION = {
  discover_route: [KIND.AGENT_DISCOVER, 'ok'], repair_script: [KIND.AGENT_REPAIR, 'warn'],
  validate_candidate: [KIND.VALIDATE_CANDIDATE, 'info'], run_scenario: [KIND.RUN_SCENARIO, 'info'],
  raise_cadence: ['검사 주기 조정', 'info'], quarantine: ['불안정, 잠시 제외', 'warn'],
};
const CLASS = { P0: '매우 중요', P1: '중요', P2: '보통' };
const BROWSER = { lightpanda: '가벼운 브라우저', chromium: 'Chromium' };
const STEP = {
  goto: '페이지 이동', click: '클릭', fill: '입력', type: '입력', press: '키 입력', select: '선택', hover: '마우스 올리기', wait_for: '화면 표시 기다림', wait_ms: '잠시 기다림', wait_url: '주소 변경 기다림',
  assert_text: '문구 확인', assert_no_text: '문구 없음 확인', assert_visible: '표시 확인', assert_not_visible: '숨김 확인', assert_url: '주소 확인', assert_count: '개수 확인', assert_request: '서버 응답 확인', assert_attr: '상태 확인', expect_popup: '새 탭 열림 확인', eval: '값 확인', use_flow: '공통 절차', screenshot: '화면 캡처',
};
const DECISION = { NEW_SCRIPT: ['새 검사 항목 제안', 'ok'], PATCH_SCRIPT: ['검사 수리안 제안', 'ok'], APP_FAILURE: ['서비스 문제 발견', 'bad'], NO_NEW_COVERAGE: ['추가할 검사 없음', 'muted'], NEEDS_REVIEW: ['사람 확인 필요', 'warn'], ORACLE_UNKNOWN: ['판단 근거 없음', 'warn'] };
const INCIDENT = { ENVIRONMENT: ['검사 환경 문제', 'warn'], APP_REGRESSION: ['서비스 결함 의심', 'bad'] };
// The kind chip already says what this is, so the stored title's technical prefix is noise.
const incTitle = t => String(t || '').replace(/^(APP_REGRESSION|ENVIRONMENT)\s*:\s*/, '');
const ORIGIN = { seed: ['파일로 등록', 'muted'], agent: ['AI 생성', 'info'], import: ['가져옴', 'muted'], human: ['사람 작성', 'muted'] };
const WHO = { seed: '파일로 등록', agent: 'AI 도우미', repair: 'AI 수리', system: '시스템', human: '사람' };
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
// Go 기간 표기("6h", "1h30m", "0s")를 한국어로 읽습니다. 0만 있는 값은 빈 문자열이
// 되어, 부르는 쪽이 "0초마다" 같은 틀린 안내 대신 주기 문구를 통째로 뺄 수 있습니다.
const DUR_KO = { h: '시간', m: '분', s: '초' };
function dur(v) {
  const parts = String(v ?? '').match(/\d+(?:\.\d+)?[hms]/g) || [];
  const kept = parts.filter(t => parseFloat(t) > 0);
  return kept.map(t => t.slice(0, -1) + DUR_KO[t.slice(-1)]).join(' ');
}
const flowOf = (n) => { const m = /^flow:([^/]+)\//.exec(n || ''); return m ? m[1] : ''; };

// ---- 실패한 요청을 사람의 문장으로 -------------------------------------------
// 서버는 오류 본문의 첫 줄에 사람이 읽을 한국어 문장을, 그다음 줄에 개발자용
// 원문을 싣습니다. 화면에는 첫 줄만 문장으로 보여 주고 원문은 개발자 정보
// 모드에서만 드러내, 실패한 순간에만 영어 조각이 튀어나오지 않게 합니다.
async function httpFail(r, fallback) {
  let body = '';
  try { body = (await r.text()).trim(); } catch (_) { body = ''; }
  try { const j = JSON.parse(body); if (j && j.error) body = String(j.error); } catch (_) { /* 본문이 JSON 이 아니면 그대로 씁니다 */ }
  const lines = body.split('\n');
  const head = (lines[0] || '').trim();
  const human = head && !/^[[{<]/.test(head) && !/^HTTP\b/.test(head) ? head : (fallback || '요청을 처리하지 못했습니다.');
  const e = new Error(human);
  e.detail = lines.slice(1).join('\n').trim() || (human === head ? '' : head);
  e.status = r.status;
  return e;
}
// 오류 문장 뒤에 원문을 dev() 로 붙입니다. 원문이 없으면 문장만 남습니다.
const errHTML = (e) => `${esc((e && e.message) || '요청을 처리하지 못했습니다.')}${e && e.detail ? ' ' + dev(e.detail) : ''}`;

// ---- toast -----------------------------------------------------------------
function toast(msg) { const t = $('toast'); if (!t) return; t.textContent = msg; t.classList.add('show'); clearTimeout(t._h); t._h = setTimeout(() => t.classList.remove('show'), 3000); }

// ---- polling with change detection ------------------------------------------
// The server tags polled payloads with an ETag that ignores the wall clock.
// When the tag repeats, the page skips its re-render, so an open <details>,
// a text selection or a scrolled list is not wiped every few seconds.
// owner: 이 폴러가 화면의 실시간 표시(#live, #freshText)와 연결 오류 자리(#fetchErr)를
// 맡는지를 정합니다. 한 페이지에서 고유 폴러 외에 보조 폴러(예: 레일 배지 갱신)를
// 더 둘 때는 owner:false 를 주어, 보조 폴러가 그 표시를 건드리지 않게 합니다.
function poller({ url, every, onData, onError, tick, owner = true }) {
  let etag = null, lastOk = 0, timer = null, inflight = null;
  // inflight 는 불리언이 아니라 진행 중인 요청 그 자체입니다. 이미 돌고 있으면 그
  // 약속을 그대로 돌려주므로, await refresh() 를 한 쪽은 어느 경우에도 "화면이 다시
  // 그려진 뒤"에 이어서 일할 수 있습니다. 불리언이던 시절에는 폴링과 겹치는 순간
  // refresh 가 즉시 반환해, 뒤늦게 도착한 응답이 복원해 둔 포커스를 지웠습니다.
  function refresh(force) {
    if (inflight) return inflight;
    inflight = run(force).finally(() => { inflight = null; });
    return inflight;
  }
  async function run(force) {
    try {
      const r = await fetch(typeof url === 'function' ? url() : url, { cache: 'no-cache' });
      if (!r.ok) throw new Error('HTTP ' + r.status);
      const tag = r.headers.get('ETag');
      const changed = force || !tag || tag !== etag;
      etag = tag;
      const data = await r.json();
      lastOk = Date.now();
      if (owner && $('live')) $('live').classList.remove('off');
      if (owner && $('freshText')) $('freshText').textContent = '실시간 갱신 ' + new Date(data.now || lastOk).toLocaleTimeString('ko-KR', { hour: '2-digit', minute: '2-digit', second: '2-digit' });
      if (owner && $('fetchErr')) $('fetchErr').innerHTML = '';
      onData(data, changed);
    } catch (e) {
      if (owner && $('live')) $('live').classList.add('off');
      if (owner && $('freshText')) $('freshText').textContent = lastOk ? `갱신 실패, 마지막 성공 ${ago(new Date(lastOk).toLocaleString('sv-SE'))}` : '연결 실패';
      if (onError) onError(e);
      else if (owner && $('fetchErr')) {
        $('fetchErr').innerHTML = `<div class="errbox"><span>현황 서버에 연결하지 못했습니다 (${esc(e.message)}). vigil가 실행 중인지 확인해 주세요.</span><button class="btn" id="retryBtn">다시 시도</button></div>`;
        // 결선은 반드시 이 블록 안에 둡니다. 밖에 두면 배지 폴러처럼 owner:false 인
        // 보조 폴러가 남이 그린 '다시 시도' 버튼을 자기 refresh 로 가로채, 버튼이
        // 화면 데이터 대신 배지 숫자만 다시 읽습니다.
        const rb = $('retryBtn'); if (rb) rb.onclick = () => refresh(true);
      }
    }
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
// Clickable table rows respond to Enter/Space, so the dashboard works without a
// mouse and screen readers announce them as buttons. 한 목록 안에서는 한 행만
// Tab 순서에 들어가고(로빙 탭인덱스), 그 안에서는 화살표와 j/k 로 옮겨 다닙니다.
// 이동만으로는 아무것도 열리지 않으므로 처음 쓰는 사람의 동작은 그대로입니다.
const rovingAt = new Map(); // 목록 선택자 -> 지금 Tab 순서에 든 행의 번호
function bindRows(selector, handler) {
  const rows = [...document.querySelectorAll(selector)];
  if (!rows.length) return;
  let cur = Math.min(rovingAt.get(selector) ?? 0, rows.length - 1);
  const mark = (i) => { cur = i; rovingAt.set(selector, i); rows.forEach((r, k) => { r.tabIndex = k === i ? 0 : -1; }); };
  rows.forEach((tr, i) => {
    tr.tabIndex = i === cur ? 0 : -1;
    tr.setAttribute('role', 'button');
    tr.onclick = () => handler(tr);
    // 재렌더 뒤 페이지가 직접 포커스를 되돌리는 경우(검사 기록의 data-id 복원)에도
    // 그 행이 Tab 순서의 한 자리를 차지하도록 맞춥니다.
    tr.addEventListener('focus', () => { if (cur !== i) mark(i); });
    tr.onkeydown = (e) => {
      if (e.isComposing) return;
      if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); handler(tr); return; }
      if (e.metaKey || e.ctrlKey || e.altKey) return;
      let to = null;
      if (e.key === 'ArrowDown' || e.key === 'j') to = i + 1;
      else if (e.key === 'ArrowUp' || e.key === 'k') to = i - 1;
      else if (e.key === 'Home') to = 0;
      else if (e.key === 'End') to = rows.length - 1;
      if (to === null) return;
      e.preventDefault();
      const n = Math.max(0, Math.min(to, rows.length - 1));
      mark(n);
      rows[n].focus();
    };
  });
}

// ---- 단축키: 기본 화면을 바꾸지 않는 덧칠 ------------------------------------
// 매일 같은 화면을 여는 사람이 손을 마우스로 옮기지 않아도 되게 하는 층입니다.
// 발견 경로는 레일 아래 한 줄과 /help 의 단축키 표뿐이고, 첫 화면에는 배너나
// 힌트를 띄우지 않습니다.
const GOTO = { t: ['/', '할 일'], r: ['/results', '검증 결과'], s: ['/scripts', '검사 스크립트'], a: ['/activity', '시스템 활동'], h: ['/help', '도움말'] };
const SHORTCUTS = [
  ['/', '검색창으로 이동합니다 (검색이 있는 화면).'],
  ['g 다음 t', '할 일 화면으로 이동합니다.'],
  ['g 다음 r', '검증 결과 화면으로 이동합니다.'],
  ['g 다음 s', '검사 스크립트 화면으로 이동합니다.'],
  ['g 다음 a', '시스템 활동 화면으로 이동합니다.'],
  ['g 다음 h', '도움말 화면으로 이동합니다.'],
  ['↓ ↑ 또는 j k', '목록에서 아래위 줄로 포커스를 옮깁니다. 옮기기만 하고 열지는 않습니다.'],
  ['Home / End', '목록의 첫 줄과 마지막 줄로 갑니다.'],
  ['Enter 또는 Space', '포커스가 있는 줄을 엽니다.'],
  ['←  →', '증거 탭 사이를 옮깁니다.'],
  ['?', '이 단축키 목록을 엽니다.'],
  ['Esc', '열려 있는 확인 패널을 닫고, 없으면 이 목록을 닫습니다.'],
];
// 페이지가 자기 오버레이(승인 확인 팝오버 같은 것)를 열고 있는 동안에는 Escape 를
// 가로채지 않겠다고 알리는 자리입니다. 팝오버가 단축키 목록보다 먼저입니다.
const escGuards = [];
function addEscGuard(fn) { escGuards.push(fn); }
const escBlocked = () => escGuards.some(f => { try { return !!f(); } catch (_) { return false; } });

let kbdBox = null, kbdReturn = null;
function shortcutBox() {
  if (kbdBox) return kbdBox;
  kbdBox = document.createElement('dialog');
  kbdBox.className = 'kbdbox';
  kbdBox.id = 'kbdBox';
  kbdBox.innerHTML = `<h2>단축키</h2>
    <table class="kbdlist"><caption class="sr-only">키보드 단축키 목록</caption><tbody>${SHORTCUTS.map(([k, w]) => `<tr><td><kbd>${esc(k)}</kbd></td><td>${esc(w)}</td></tr>`).join('')}</tbody></table>
    <p class="sub">입력창에 글자를 쓰는 동안과 한글을 조합하는 동안에는 단축키가 동작하지 않습니다.</p>
    <div class="kbdfoot"><a href="/help#keys">도움말의 단축키 표</a><button type="button" class="btn" id="kbdClose">닫기</button></div>`;
  document.body.appendChild(kbdBox);
  kbdBox.querySelector('#kbdClose').onclick = () => closeShortcuts();
  kbdBox.addEventListener('close', () => {
    if (kbdReturn && document.contains(kbdReturn)) kbdReturn.focus();
    kbdReturn = null;
  });
  return kbdBox;
}
function openShortcuts() {
  const d = shortcutBox();
  if (d.open) return;
  kbdReturn = document.activeElement;
  if (d.showModal) d.showModal(); else d.setAttribute('open', '');
}
function closeShortcuts() { if (kbdBox && kbdBox.open) kbdBox.close(); }

let gAt = 0; // 'g' 를 누른 시각. 1.5초 안에 다음 글자가 와야 이동으로 읽습니다.
document.addEventListener('keydown', (e) => {
  // 한글을 조합하는 동안에는 어떤 단축키도 발동하지 않습니다.
  if (e.isComposing || e.keyCode === 229) return;
  if (e.key === 'Escape') {
    if (escBlocked()) return; // 팝오버가 먼저입니다. 페이지 처리에 맡깁니다.
    if (kbdBox && kbdBox.open) { e.preventDefault(); e.stopImmediatePropagation(); closeShortcuts(); }
    return;
  }
  if (e.metaKey || e.ctrlKey || e.altKey) return;
  const t = e.target;
  if (t && (t.isContentEditable || /^(INPUT|TEXTAREA|SELECT)$/.test(t.tagName || ''))) return;
  if (kbdBox && kbdBox.open) return;
  const now = Date.now();
  if (gAt && now - gAt < 1500) {
    gAt = 0;
    const go = GOTO[(e.key || '').toLowerCase()];
    if (go) { e.preventDefault(); location.href = go[0]; return; }
  }
  if (e.key === 'g') { gAt = now; return; }
  gAt = 0;
  if (e.key === '/') { const q = $('q'); if (q) { e.preventDefault(); q.focus(); q.select(); } return; }
  if (e.key === '?') { e.preventDefault(); openShortcuts(); }
});
// 레일 아래 한 줄이 유일한 발견 경로입니다. 모든 화면의 레일에 같은 자리로 붙습니다.
(function installShortcutHint() {
  const rail = document.querySelector('.rail');
  if (!rail || $('kbdHint')) return;
  const b = document.createElement('button');
  b.type = 'button';
  b.className = 'rail-kbd';
  b.id = 'kbdHint';
  b.innerHTML = '단축키 <kbd>?</kbd>';
  b.onclick = () => openShortcuts();
  const foot = rail.querySelector('.rail-foot');
  if (foot) rail.insertBefore(b, foot); else rail.appendChild(b);
})();

// ---- 정렬: 누르지 않으면 지금 순서 그대로 ------------------------------------
// 숫자와 시각 열 머리글만 button 으로 감싸고 aria-sort 를 붙입니다. 상태는 모듈
// 변수와 URL 의 sort 파라미터에 함께 남으므로 폴링 재렌더에서 원복되지 않습니다.
const ARIA_SORT = { asc: 'ascending', desc: 'descending' };
function sortTh(state, key, label, cls) {
  const dir = state && state.key === key ? state.dir : '';
  const arrow = dir === 'asc' ? '▲' : dir === 'desc' ? '▼' : '↕';
  return `<th scope="col"${cls ? ` class="${cls}"` : ''} aria-sort="${ARIA_SORT[dir] || 'none'}"><button type="button" class="sortbtn" data-sort="${esc(key)}">${esc(label)}<span class="sarr" aria-hidden="true">${arrow}</span></button></th>`;
}
// 같은 머리글을 다시 누르면 내림차순 → 오름차순 → 기본 순서로 돌아갑니다.
function nextSort(state, key) {
  if (!state || state.key !== key) return { key, dir: 'desc' };
  return state.dir === 'desc' ? { key, dir: 'asc' } : null;
}
const sortParam = (s) => (s ? `${s.key}:${s.dir}` : '');
function parseSort(v) {
  const [k, d] = String(v || '').split(':');
  return k && (d === 'asc' || d === 'desc') ? { key: k, dir: d } : null;
}
// 값이 없는 행은 방향과 상관없이 뒤로 보냅니다. 같은 값이면 원래 순서를 지킵니다.
function sortRows(rows, state, keys) {
  if (!state || !keys[state.key]) return rows;
  const f = keys[state.key], m = state.dir === 'asc' ? 1 : -1;
  const blank = (v) => v === null || v === undefined || v === '';
  return rows.map((r, i) => [r, i]).sort((a, b) => {
    const x = f(a[0]), y = f(b[0]);
    if (blank(x) && blank(y)) return a[1] - b[1];
    if (blank(x)) return 1;
    if (blank(y)) return -1;
    if (x === y) return a[1] - b[1];
    return (x > y ? 1 : -1) * m;
  }).map(p => p[0]);
}
function bindSort(root, get, set) {
  (root || document).querySelectorAll('button[data-sort]').forEach(b => {
    b.onclick = (e) => { e.stopPropagation(); set(nextSort(get(), b.dataset.sort)); };
  });
}

// 타자마다 주소를 고쳐 쓰지 않도록 검색어 반영에 쓰는 지연 실행입니다.
function debounce(fn, ms) {
  let h = null;
  return (...a) => { clearTimeout(h); h = setTimeout(() => fn(...a), ms); };
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

// ---- re-render that keeps the reader's place --------------------------------
// Pages rebuild whole panels with innerHTML. That throws away two things the
// reader was using: the button under the keyboard focus and the scroll position
// of every box on the page (the script detail, the YAML block, the AI activity
// list). rerender() takes a snapshot right before the rebuild and puts both
// back afterwards.
//
//   rerender(fn)                 사용자 조작이 부른 재렌더. 스크롤을 되돌리고,
//                                누르고 있던 요소가 사라졌으면 포커스도 되돌립니다.
//   rerender(fn, {focus:false})  폴링이 부른 자동 재렌더. 스크롤만 보존하고
//                                포커스는 건드리지 않습니다.
//
// fn 이 스스로 포커스를 옮겼다면(예: 취소 뒤 원래 버튼으로 돌아가는 흐름)
// 그 결정을 그대로 둡니다. 포커스가 body 로 떨어진 경우에만 복원합니다.

// 재렌더 뒤에도 같은 요소를 찾을 수 있는 식별자. id 가 가장 튼튼하고,
// 없으면 각 화면이 쓰는 data-* 조작 키를 씁니다.
const FOCUS_KEYS = ['act', 'open', 'detail', 'tab', 'sort', 'f', 'c', 'g', 'more', 'req', 'v', 'marker', 'id', 'dir', 'retry'];
function focusSelector(el) {
  if (!el || el.nodeType !== 1 || el === document.body) return null;
  if (el.id) return '#' + CSS.escape(el.id);
  for (const k of FOCUS_KEYS) {
    const v = el.dataset ? el.dataset[k] : undefined;
    if (v !== undefined) return `[data-${k}="${CSS.escape(v)}"]`;
  }
  return null;
}
// 부모를 따라 올라가며 id 를 가진 조상까지의 경로를 만듭니다. id 없는 스크롤
// 상자(.acts 같은 것)를 재렌더 뒤에 다시 찾는 데 씁니다.
function nodePath(el) {
  const parts = [];
  let n = el;
  while (n && n.nodeType === 1 && !n.id) {
    const p = n.parentElement;
    if (!p) return null;
    const same = [...p.children].filter(c => c.tagName === n.tagName);
    parts.unshift(`${n.tagName.toLowerCase()}:nth-of-type(${same.indexOf(n) + 1})`);
    n = p;
  }
  if (!n || n.nodeType !== 1) return null;
  return '#' + CSS.escape(n.id) + (parts.length ? ' > ' + parts.join(' > ') : '');
}
function scrollSnapshot() {
  const boxes = [];
  document.querySelectorAll('*').forEach(el => {
    if (el.scrollTop > 0) { const k = el.id ? '#' + CSS.escape(el.id) : nodePath(el); if (k) boxes.push([k, el.scrollTop]); }
  });
  return { boxes, y: window.scrollY };
}
function scrollRestore(snap) {
  for (const [k, top] of snap.boxes) { const el = document.querySelector(k); if (el && el.scrollTop !== top) el.scrollTop = top; }
  if (window.scrollY !== snap.y) window.scrollTo(0, snap.y);
}
// 재렌더 도중 화면이 "여기로 옮겨 가겠다"고 선언하는 자리입니다. rerender 의 스크롤
// 복원은 읽던 자리를 지킨다는 뜻이지 의도한 이동까지 되돌린다는 뜻이 아닙니다.
// 움직임 줄이기를 켜면 scrollIntoView 가 동기라 복원이 곧바로 되돌려 버리므로,
// scrollY 비교 같은 우연한 가드에 기대지 않고 플래그로 구분합니다.
let scrollClaimed = false;
function claimScroll(el, opts) { if (!el) return; scrollClaimed = true; el.scrollIntoView(opts); }
function rerender(fn, opts) {
  const keepFocus = !(opts && opts.focus === false);
  const prev = document.activeElement;
  const sel = keepFocus ? focusSelector(prev) : null;
  // 필터 때문에 누른 것이 아예 사라진 경우에 대비해, 그 요소를 담고 있던 가장
  // 가까운 조상(패널이나 묶음)을 미리 적어 둡니다. 자기 자신은 제외합니다.
  const fallback = keepFocus && prev && prev.nodeType === 1 && prev.parentElement ? nodePath(prev.parentElement.closest('[id]') || prev.parentElement) : null;
  const snap = scrollSnapshot();
  try { fn(); } finally {
    if (!scrollClaimed) scrollRestore(snap);
    scrollClaimed = false;
    const now = document.activeElement;
    const loose = !now || now === document.body || now === document.documentElement || !document.contains(now);
    if (keepFocus && sel && loose) {
      const el = document.querySelector(sel) || (fallback ? document.querySelector(fallback) : null);
      if (el) { if (!el.hasAttribute('tabindex') && el.tabIndex < 0) el.tabIndex = -1; el.focus({ preventScroll: true }); }
    }
  }
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
  const stepLine = row.failed_step ? `멈춘 곳: ${row.failed_step}단계 ${STEP[row.failed_action] || row.failed_action || ''}` : '';
  const step = stepLine ? `<p class="sub">${esc(stepLine)}</p>` : '';
  const l3 = c.detail ? `<details class="cause-l3" data-key="tech:${esc(key)}"${prefs.dev ? ' open' : ''}><summary>기술 정보</summary><pre class="raw">${esc(c.detail)}</pre></details>` : '';
  // 한 건만 이슈나 메일로 옮기려는 사람이 화면에서 본 문장을 그대로 가져갑니다.
  const text = [c.headline, c.why, stepLine, c.next].filter(Boolean).join('\n');
  const copy = text ? `<p class="cause-copy"><button type="button" class="btn" data-cause="${esc(text)}">원인 복사</button></p>` : '';
  return `<div class="cause">
    <p class="cause-l1">${st(CAUSE_KIND, c.kind)}<span class="cause-line">${esc(c.headline)}</span></p>
    <details class="cause-l2" data-key="cause:${esc(key)}"><summary>자세히</summary><div class="cause-body">${c.why ? `<p>${esc(c.why)}</p>` : ''}${step}${extra || ''}${c.next ? `<p class="cause-next">${esc(c.next)}</p>` : ''}${copy}${l3}</div></details>
  </div>`;
}
// A cause block may sit inside a clickable row; its toggles must not also open
// the row. Remembers open layers across re-renders like every other <details>.
function bindCause(root) {
  (root || document).querySelectorAll('.cause summary').forEach(s => s.addEventListener('click', (e) => e.stopPropagation()));
  (root || document).querySelectorAll('.cause button[data-cause]').forEach(b => b.onclick = async (e) => {
    e.stopPropagation();
    try { await navigator.clipboard.writeText(b.dataset.cause); toast('원인 문장을 복사했습니다.'); }
    catch { toast('복사에 실패했습니다. 문장을 선택해 복사해 주세요.'); }
  });
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
// 같은 코드가 맥락에 따라 다른 뜻을 갖는 경우에만 덮어쓰는 사전입니다. 예를 들어
// NEEDS_REVIEW 는 스크립트 상태로 쓰이면 '수리에 실패했다'는 뜻이고, 실행 결과로
// 쓰이면 '이번 실행만으로는 판정할 수 없다'는 뜻입니다.
const GLOSS_CTX = new Map([
  [OUTCOME, { NEEDS_REVIEW: '이번 실행만으로는 판단할 수 없습니다. 증거를 보고 사람이 결론을 내려 주세요.' }],
]);
// glossOf 는 맥락 사전을 먼저 찾고, 없으면 공용 GLOSS 를 씁니다.
const glossOf = (map, code) => (GLOSS_CTX.get(map) || {})[code] || GLOSS[code] || '';

// tip renders a term with its plain-language sentence: visible label, tooltip on
// hover, and the same sentence in the text itself. A span has no role of its
// own, so aria-label on it may be dropped; sr-only text always reaches readers.
const tip = (label, explain) => explain
  ? `<span class="tip" tabindex="0" title="${esc(explain)}">${esc(label)}<span class="sr-only">, ${esc(explain)}</span></span>`
  : esc(label);
// stTip is a status chip that also carries its glossary sentence. When there is
// a sentence the chip takes a dotted underline and a tab stop, so keyboard
// users can read the same explanation the mouse tooltip shows.
const stTip = (map, v) => {
  const m = map[v], g = glossOf(map, v);
  if (!v) return '';
  const label = m ? m[0] : v;
  return `<span class="st st-${m ? m[1] : 'muted'}${g ? ' has-gloss' : ''}"${g ? ` tabindex="0" title="${esc(g)}"` : ''}><i></i>${esc(label)}${g ? `<span class="sr-only">, ${esc(g)}</span>` : ''}</span>`;
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
// A count next to a rail link. 배지는 '사람이 손대야 하는 건수'만 말합니다.
// 숫자만 놓으면 무엇의 수인지 알 수 없으므로 label 을 받아 aria-label 에
// '승인 대기 3건'처럼 적습니다. 값이 없거나 0이면 잘못된 0 대신 비워 둡니다.
function railCount(id, n, label) {
  const el = $(id); if (!el) return;
  const v = Number(n);
  const show = Number.isFinite(v) && v > 0;
  el.textContent = show ? v : '';
  if (show && label) el.setAttribute('aria-label', `${label} ${v}건`);
  else el.removeAttribute('aria-label');
}
// The same three numbers, in the same places, on every page.
function railCounts(counts) {
  const c = counts || {};
  railCount('nTodo', c.total, '확인할 일');
  railCount('nBad', c.app_failure, '재현된 문제');
  railCount('nScripts', c.pending_approval, '승인 대기');
}
// Pages that do not poll /api/todo themselves keep the badges honest with one
// light poll. owner:false 로 두어 페이지 고유 폴러의 실시간 표시와 오류
// 자리를 건드리지 않습니다. 불러오지 못하면 배지를 그대로 두고 조용히 넘어갑니다.
function railBadgePoll(every) {
  if (!$('nTodo') && !$('nBad') && !$('nScripts')) return null;
  const p = poller({
    url: '/api/todo', every: every || 30000, owner: false,
    onData: (d) => railCounts(d.counts), onError: () => {},
  });
  p.start();
  return p;
}

// ---- collapsible group state (survives re-render) ----------------------------
// Groups remember open/closed per page in localStorage, keyed by name.
function groupOpen(page, key, fallback) {
  const v = store.get(`vigil.group.${page}.${key}`);
  return v === null ? fallback : v === '1';
}
function setGroupOpen(page, key, open) { store.setOn(`vigil.group.${page}.${key}`, open); }
