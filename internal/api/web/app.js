'use strict';
const $ = (s) => document.querySelector(s);
const $$ = (s) => document.querySelectorAll(s);

// S keeps what several pages need: the chain height, block time, the
// waiting likes (by id), the smallest like, the anonymous membership.
const S = { page: 'board', height: 0, blockSec: 60, queued: new Map(), min: '0', rln: {}, quote: null, status: null };
let board = '';
let openThread = '';

async function api(path, opts = {}) {
  opts.headers = Object.assign({ 'X-Chainboard': '1' }, opts.headers || {});
  const r = await fetch(path, opts);
  const j = await r.json().catch(() => ({}));
  if (!r.ok) { const e = new Error(j.error || r.statusText); e.status = r.status; e.body = j; throw e; }
  return j;
}
const post = (path, body) => api(path, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body) });

// The node refuses a surcharged send, the last anonymous post of an epoch,
// accepting a bounty offer and the like with 409 and the warnings; they
// must be accepted.
function accepted(e) {
  if (e.status !== 409 || !e.body || !e.body.warnings) throw e;
  const lines = e.body.warnings.map((w) => '· ' + w.message);
  if (e.body.fee && e.body.fee !== '0') lines.push('예상 수수료: ' + e.body.fee + ' 코인');
  return confirm(lines.join('\n') + '\n\n그래도 보낼까요?');
}
async function send(path, body) {
  try { return await post(path, body); } catch (e) {
    if (!accepted(e)) throw new Error('보내지 않았어요');
    return post(path, Object.assign({}, body, { confirm: true }));
  }
}
async function upload(file, tier) {
  const up = (confirmed) => { const fd = new FormData(); fd.append('file', file); fd.append('tier', tier);
    if (confirmed) fd.append('confirm', 'true'); return api('/api/upload', { method: 'POST', body: fd }); };
  try { return await up(false); } catch (e) { if (!accepted(e)) throw new Error('보내지 않았어요'); return up(true); }
}

const PERM = 65535;
function tierName(t) { return t === PERM ? '영구' : t === 1 ? '1개월' : (t % 12 === 0 ? (t / 12) + '년' : t + '개월'); }
function coin(units) { return (Number(units) / 1e8).toFixed(4).replace(/\.?0+$/, ''); }
function num(s) { return String(s).replace(/(\.\d*?)0+$/, '$1').replace(/\.$/, ''); }
function el(tag, cls, text) { const e = document.createElement(tag); if (cls) e.className = cls; if (text !== undefined) e.textContent = text; return e; }
function act(text, fn) { const a = el('a', 'act', text); a.onclick = fn; return a; }
function short(s) { return s ? s.slice(0, 10) + '…' : ''; }
function when(t) { return t ? new Date(t * 1000).toLocaleString() : '미확정'; }
function toast(msg, bad) { const s = $('#status'); s.textContent = msg; s.className = 'small ' + (bad ? 'err' : 'muted'); S.toastUntil = Date.now() + 6000; }
const fail = (e) => toast(e.message, true);
function kv(box, rows) {
  box.replaceChildren();
  for (const [k, v] of rows) { const d = el('div'); d.append(el('span', 'k', k), el('span', '', String(v))); box.append(d); }
}
function timeLeft(blocks) {
  const m = Math.round(blocks * S.blockSec / 60);
  return m < 90 ? `약 ${m}분` : m < 2880 ? `약 ${Math.round(m / 60)}시간` : m < 2 * 525600 ? `약 ${Math.round(m / 1440)}일` : `약 ${Math.round(m / 525600)}년`;
}
function empty(box, text) { if (!box.children.length) box.append(el('div', 'empty', text)); }
// Images arrive as base64 bytes; the type comes from their first bytes.
function dataURL(b64) {
  const head = atob(b64.slice(0, 8));
  const t = head.startsWith('\x89PNG') ? 'png' : head.startsWith('GIF8') ? 'gif' : head.startsWith('\xff\xd8') ? 'jpeg' : '';
  return t ? `data:image/${t};base64,${b64}` : '';
}

// retention draws how long something is kept: full for permanent, empty
// once gone, otherwise the share of its kept time still left.
function retention(label, tier, from, until, gone) {
  const d = el('div', 'keep');
  const bar = el('div', 'bar'); const fill = el('div'); bar.append(fill);
  let text;
  if (gone) { d.classList.add('gone'); text = `${label} 보관 끝남 · 지워졌어요`; }
  else if (tier === PERM || !until) { d.classList.add('perm'); fill.style.width = '100%'; text = `${label} 영구 보관`; }
  else {
    const left = until > S.height ? until - S.height : 0;
    const total = Math.max(1, until - Math.min(from || until, S.height));
    fill.style.width = Math.min(100, left / total * 100).toFixed(1) + '%';
    // Storage is bought in buckets, so it is kept at least the tier, often longer.
    text = left ? `${label} ${tierName(tier)} 이상 보관 · ${timeLeft(left)} 남음 (높이 ${until}까지)` : `${label} 보관 끝남`;
    if (!left) d.classList.add('gone');
  }
  d.title = text;
  d.append(el('div', '', text), bar);
  return d;
}

// ---------- pages ----------
function showPage(name) {
  if (!document.getElementById('page-' + name)) name = 'board';
  S.page = name;
  for (const p of $$('.page')) p.hidden = p.id !== 'page-' + name;
  for (const a of $$('#nav a')) a.classList.toggle('on', a.dataset.page === name);
  if (location.hash !== '#' + name) history.replaceState(null, '', '#' + name);
  refreshPage().catch(() => {});
}
for (const a of $$('#nav a')) a.onclick = () => showPage(a.dataset.page);
document.addEventListener('click', (e) => { const g = e.target.closest('[data-goto]'); if (g) showPage(g.dataset.goto); });
window.onhashchange = () => showPage(location.hash.slice(1));

async function refreshStatus() {
  const s = await api('/api/status');
  S.status = s; S.height = s.height; S.blockSec = s.block_seconds || 60;
  $('#ver').textContent = `v${s.version} · ${s.network}`;
  if (!S.toastUntil || Date.now() > S.toastUntil) {
    $('#status').className = 'small muted';
    $('#status').textContent = `높이 ${s.height} · 피어 ${s.peers} · 저장 가격 ${s.storage_price} · 대기 ${s.mempool}`;
  }
  S.rln = await api('/api/rln');
  if (!S.me) S.me = (await api('/api/wallet')).address;
  const lq = await api('/api/likes');
  S.likes = lq; S.min = lq.min_amount;
  S.queued = new Map((lq.likes || []).map((l) => [l.id, l]));
}

async function refreshPage() {
  switch (S.page) {
    case 'board': return Promise.all([refreshBoards(), refreshPosts(), openThread ? showThread(openThread) : null, anonHint()]);
    case 'funds': return refreshFunds();
    case 'bounties': return refreshBounties();
    case 'wallet': return refreshWallet();
    case 'activity': return refreshActivity();
    case 'settings': return refreshSettings();
  }
}

// ---------- board ----------
async function refreshBoards() {
  const bs = await api('/api/boards');
  const ul = $('#boards'); ul.replaceChildren();
  Object.entries(bs).sort((a, b) => b[1] - a[1]).forEach(([name, n]) => {
    const li = el('li', name === board ? 'on' : '', `${name} (${n})`); li.onclick = () => selectBoard(name); ul.append(li);
  });
  empty(ul, '아직 게시판이 없어요. 이름을 적어 열어 보세요.');
}

function anonHint() {
  const r = S.rln; const box = $('#anonHint'); box.replaceChildren();
  if (r.status !== 'active') { box.append(el('div', '', r.status === 'pending' ? '보증금 등록이 처리되는 중이에요.' : '등록하지 않았어요.')); return; }
  const per = epochName();
  box.append(el('div', '', `${per} ${Math.min(r.limit, r.cap)}개 중 ${r.remaining}개 남음 · ${r.epoch_minutes_left}분 뒤 충전`));
  if (r.remaining === 1) box.append(el('div', 'warn', '다음 익명 글이 이번 시간대의 마지막이에요.'));
  if (r.remaining === 0) box.append(el('div', 'warn', `이번 시간대 한도를 다 썼어요.`));
}
function epochName() {
  const m = S.rln.epoch_minutes || 60;
  return m === 60 ? '1시간에' : m < 60 ? `${m}분에` : `${m / 60}시간에`;
}

function likeLabel(p) {
  return `좋아요 ${p.likes}${p.liked ? ' ✓' : ''}` + (Number(p.tips) > 0 ? ` · 후원 ${coin(p.tips)} 코인` : '');
}

// A line holding only [[n]] shows attachment n there; attachments no line
// names follow the text. (The marker is plain text on chain: a few bytes.)
const MARK = /^\s*\[\[([1-4])\]\]\s*$/;
function attachment(p, i) {
  const a = p.attach[i];
  const box = el('div');
  const img = el('img'); img.src = '/api/data/' + a; img.alt = `첨부 ${i + 1}`; img.loading = 'lazy';
  img.onerror = () => img.replaceWith(el('div', 'small muted', '첨부를 이 노드가 아직 갖고 있지 않아요 · ' + short(a)));
  box.append(img);
  const k = (p.kept || [])[i];
  if (k) {
    const r = retention(`첨부 ${i + 1}`, k.tier, k.from, k.until, k.gone);
    if (!k.gone && k.tier !== PERM) r.append(act('첨부 연장', () => extend({ root: a }, '5y')));
    box.append(r);
  }
  return box;
}
function renderBody(d, p) {
  const atts = p.attach || [];
  const shown = new Set();
  if (p.text_state === 'expired') d.append(el('div', 'text muted', '보관 기간이 끝나 본문이 지워진 글이에요.'));
  else if (p.text_state === 'loading') d.append(el('div', 'text muted', '본문을 불러오는 중…'));
  else {
    let lines = [];
    const flush = () => { const t = lines.join('\n'); if (t.trim()) d.append(el('div', 'text', t.replace(/^\n+|\n+$/g, ''))); lines = []; };
    for (const line of (p.text || '').split('\n')) {
      const m = line.match(MARK);
      const i = m ? Number(m[1]) - 1 : -1;
      if (i >= 0 && i < atts.length) { flush(); d.append(attachment(p, i)); shown.add(i); } else lines.push(line);
    }
    flush();
  }
  if (p.tier && !p.pending) d.append(retention('글', p.tier, p.height, p.until, p.text_state === 'expired'));
  atts.forEach((_, i) => { if (!shown.has(i)) d.append(attachment(p, i)); });
}

function renderPost(p, isComment) {
  const d = el('div', 'post' + (p.pending ? ' pending' : ''));
  if (p.lane === 'bounty') d.append(el('span', 'badge live', '현상금'), ' ');
  if (p.lane === 'fund') d.append(el('span', 'badge live', '크라우드 펀딩'), ' ');
  renderBody(d, p);
  if (p.fund) d.append(renderFund(p.id, p.fund));
  const m = el('div', 'meta');
  const who = p.lane === 'anon' ? (p.pseudonym ? '가명 ' + short(p.author) : '익명') : short(p.author);
  m.append(el('span', '', who), el('span', '', when(p.time)));
  if (p.board && S.page !== 'board') m.append(act('게시판 ' + p.board, () => { selectBoard(p.board); showPage('board'); }));
  if (p.pending) m.append(el('span', '', '보내는 중 · 미확정'));
  else {
    // Liking one's own post (signed, pseudonymous or anonymous) would only
    // move coins between one's own addresses, so it is not offered.
    if (p.mine) m.append(el('span', '', `좋아요 ${p.likes}` + (Number(p.tips) > 0 ? ` · 후원 ${coin(p.tips)} 코인` : '') + ' · 내 글'));
    else if (p.tip_to) {
      const q = S.queued.get(p.id);
      m.append(act(likeLabel(p), () => openLike({ kind: 'post', id: p.id, post: p })));
      if (q) m.append(act(`후원 ${num(q.amount)} 코인 대기 · 빼기`, () => unqueue(p.id)));
    } else if (p.lane === 'bounty') m.append(act('현상금 보기', () => showPage('bounties')));
    else if (!p.fund) m.append(el('span', '', `좋아요 ${p.likes}`));
    if (p.tier && p.tier !== PERM && p.text_state === 'ok') m.append(act('글 연장', () => extend({ post: p.id }, '1y')));
    if (!isComment && p.lane !== 'bounty' && p.lane !== 'fund') m.append(act(`댓글 ${p.comments}`, () => { showThread(p.id); if (S.page !== 'board') { selectBoard(p.board, true); showPage('board'); } }));
  }
  d.append(m);
  return d;
}

function extend(target, def) {
  const tier = prompt('보관할 기간 (1y, 5y, 20y, perm) — 누구나 수수료를 내고 연장할 수 있어요', def);
  if (tier) send('/api/extend', Object.assign({ tier }, target)).then(() => toast('연장을 보냈어요')).catch(fail);
}
function unqueue(id) { post('/api/like', { id, undo: true }).then(() => { toast('대기 중인 좋아요를 뺐어요'); tick(); }).catch(fail); }

async function refreshPosts() {
  if (!board) { $('#posts').replaceChildren(el('p', 'muted', '왼쪽에서 게시판을 고르세요.')); return; }
  const min = $('#bestOnly').checked ? Number($('#minLikes').value || 1) : 0;
  const [ps, pend] = await Promise.all([api(`/api/board?name=${encodeURIComponent(board)}&min_likes=${min}`), api('/api/pending')]);
  const box = $('#posts'); box.replaceChildren();
  (pend || []).filter((p) => p.board === board && !p.parent).forEach((p) => box.append(renderPost(p)));
  (ps || []).forEach((p) => box.append(renderPost(p)));
  if (!box.children.length) box.append(el('p', 'muted', '아직 글이 없어요. 첫 글을 써 보세요.'));
}

async function showThread(id) {
  openThread = id;
  const t = await api('/api/post?id=' + id);
  const box = $('#thread'); box.replaceChildren();
  const card = el('div', 'card');
  card.append(act('닫기', () => { openThread = ''; box.replaceChildren(); }), renderPost(t.post));
  const sub = el('div', 'sub');
  (t.comments || []).forEach((c) => sub.append(renderPost(c, true)));
  card.append(sub);
  const f = el('form'); const ta = el('textarea'); ta.placeholder = '댓글'; ta.maxLength = 1000;
  const ln = el('select'); ln.append(new Option('익명', 'anon'), new Option('수수료', 'fee'));
  const row = el('div', 'row'); row.append(ln, el('button', '', '댓글 쓰기'));
  f.append(ta, row);
  f.onsubmit = (e) => { e.preventDefault(); send('/api/post', { board: t.post.board, text: ta.value, parent: id, lane: ln.value })
    .then(() => { ta.value = ''; toast('댓글을 보냈어요'); }).catch(fail); };
  card.append(f);
  box.append(card);
}

function selectBoard(name, keepThread) {
  board = name; $('#boardTitle').textContent = name;
  if (!keepThread) { openThread = ''; $('#thread').replaceChildren(); }
  refreshPosts().catch(fail); refreshBoards().catch(() => {});
}
$('#boardForm').onsubmit = (e) => { e.preventDefault(); selectBoard(e.target.name.value.trim()); e.target.reset(); };
$('#bestOnly').onchange = () => refreshPosts().catch(fail);
$('#postForm').onsubmit = async (e) => {
  e.preventDefault();
  if (!board) return toast('먼저 게시판을 고르세요', true);
  const f = e.target;
  try {
    const attach = [];
    let note = '';
    const files = [...f.file.files];
    if (files.length) {
      if (f.lane.value === 'anon') return toast('첨부는 수수료 차선에서만 올릴 수 있어요', true);
      if (files.length > 4) return toast('첨부는 글 하나에 4개까지예요', true);
      for (const file of files) {
        const r = await upload(file, f.tier.value);
        attach.push(r.root);
        if (r.action === 'reuse') note = ' 이미 저장된 파일은 다시 올리지 않았어요.';
        if (r.action === 'extend') note = ' 이미 저장된 파일은 보관 기간만 늘렸어요.';
      }
    }
    await send('/api/post', { board, text: f.text.value, lane: f.lane.value, pseudonym: f.pseudonym.checked && f.lane.value === 'anon', attach, tier: f.textTier.value });
    f.reset(); syncLane(); markChips(); toast('글을 보냈어요. 잠시 뒤 미확정 글로 보여요.' + note); refreshPosts();
  } catch (err) { fail(err); }
};
function syncLane() {
  const f = $('#postForm'); const anon = f.lane.value === 'anon';
  f.textTier.disabled = anon; f.file.disabled = anon; f.tier.disabled = anon; $('#pseudoRow').hidden = !anon;
  if (anon) f.textTier.value = '1m';
  $('#laneNote').textContent = anon
    ? '익명 글은 영지식 증명으로 "보증금을 건 회원 중 누군가"라는 것만 드러나요(증명에 몇 초). 1개월 보관되고, 좋아요(후원)는 이 글만의 일회용 주소로 받아요. 가명으로 서명하면 글들이 한 가명으로 이어져요.'
    : '수수료 글은 지갑 주소가 공개되고, 고른 기간만큼 보관돼요. 첨부 이미지는 다시 인코딩되어 메타데이터가 지워져요.';
}
$('#postForm').lane.onchange = syncLane; syncLane();
// One button per chosen picture puts its [[n]] where the cursor is.
function markChips() {
  const f = $('#postForm'); const box = $('#markChips'); box.replaceChildren();
  [...f.file.files].slice(0, 4).forEach((file, i) => {
    const b = el('button', 'plain small', `그림 ${i + 1} 넣기`); b.type = 'button'; b.title = file.name;
    b.onclick = () => {
      const ta = f.text; const at = ta.selectionStart ?? ta.value.length;
      const mark = `${at && ta.value[at - 1] !== '\n' ? '\n' : ''}[[${i + 1}]]\n`;
      ta.value = ta.value.slice(0, at) + mark + ta.value.slice(ta.selectionEnd ?? at);
      ta.focus(); ta.selectionStart = ta.selectionEnd = at + mark.length;
    };
    box.append(b);
  });
  if (f.file.files.length > 4) box.append(el('span', 'err', '4개까지만 올릴 수 있어요'));
  else if (f.file.files.length) box.append(el('span', 'muted', '넣지 않은 그림은 글 아래에 붙어요'));
}
$('#postForm').file.onchange = markChips;

// ---------- likes ----------
// A like always carries coins: a tip to a post's writer, a pledge to a
// funding request, or more for a bounty's pot. Likes count backers.
let likeCtx = null;
function openLike(ctx) {
  likeCtx = ctx;
  const f = $('#likeForm'); f.reset();
  const titles = { post: '좋아요 · 후원', fund: '크라우드 펀딩 후원', bounty: '현상금 올리기' };
  $('#likeTitle').textContent = titles[ctx.kind];
  $('#likeInfo').textContent = (ctx.text || (ctx.post && ctx.post.text) || '').slice(0, 80);
  const q = S.queued.get(ctx.id);
  if (q) f.amount.value = num(q.amount);
  $('#likeMin').textContent = `최소 ${num(S.min)} 코인`;
  const notes = [];
  if (ctx.kind === 'post') {
    const p = ctx.post;
    if (p.lane === 'anon') notes.push(p.pseudonym ? '가명 글의 후원은 그 가명 주소로 가요.' : '익명 글의 후원은 이 글만의 일회용 주소로 가요. 누가 썼는지는 드러나지 않아요.');
    if (p.liked) notes.push('이미 좋아요가 세어진 글이에요. 더 보내도 좋아요 수는 그대로예요.');
  }
  if (ctx.kind === 'fund') notes.push('마감까지 저장되지 않으면 돌아와요.');
  if (ctx.kind === 'bounty') notes.push('현상금은 요청자가 고른 제안자에게 가요. 요청자를 믿을 수 있을 때 올리세요. 지급되지 않고 마감되면 낸 비율대로 돌아와요.');
  if (q) notes.push('이미 대기 중인 좋아요가 있어요. 담으면 금액이 바뀌어요.');
  $('#likeNote').textContent = notes.join(' ');
  $('#likeDlg').showModal();
}
$('#likeDlg').onclose = async () => {
  const how = $('#likeDlg').returnValue; const ctx = likeCtx;
  if (!ctx || (how !== 'queue' && how !== 'now')) return;
  const body = { id: ctx.id, amount: $('#likeForm').amount.value.trim(), fund: ctx.kind === 'fund', bounty: ctx.kind === 'bounty' };
  try {
    if (how === 'now') { await send('/api/like', Object.assign({ now: true }, body)); toast('좋아요를 보냈어요'); }
    else { const r = await post('/api/like', body); toast(`담았어요 · ${r.waiting}개 대기 중. 지갑에서 한꺼번에 보내요`); }
  } catch (e) { fail(e); }
  tick();
};

// ---------- crowdfunding ----------
const fundStates = { new: '새 요청 (첫 후원과 함께 체인에 올라가요)', open: '모금 중', stored: '저장 완료', closed: '마감 (환불됨)' };
const mediaNames = { 1: '이미지', 2: '이미지', 3: '영상', 4: '파일' };
function renderFund(id, f, note) {
  const box = el('div', 'small');
  const raised = Number(f.raised || 0); const need = Number(f.need_for_min || 0);
  if (f.status === 'open' || f.status === 'new') {
    const bar = el('div', 'bar'); const fill = el('div'); fill.style.width = Math.min(100, need ? raised / need * 100 : 0) + '%'; bar.append(fill); box.append(bar);
  }
  const line = [fundStates[f.status] || f.status];
  if (f.status === 'open' || f.status === 'new') line.push(`${num(f.raised || '0')} / ${num(f.need_for_min || '0')} 코인 (최소 ${tierName(f.min_tier)} 저장 비용)`, `후원 ${f.backers || 0}명`);
  if (f.tier) line.push((f.status === 'open' ? '지금 올리면 ' : '산 기간 ') + tierName(f.tier));
  box.append(el('div', 'meta', line.join(' · ')));
  if (f.status === 'open' || f.status === 'new') {
    const q = S.queued.get(id);
    box.append(act(q ? `후원 ${num(q.amount)} 코인 대기 · 바꾸기` : '좋아요 · 후원', () => openLike({ kind: 'fund', id, text: note })));
  }
  return box;
}

// showFundCheck shows a request's spot check (the first chunk and three at
// random must come from the network and match its data root) in box.
async function showFundCheck(id, box = $('#fundCheck'), probe = false, compact = false) {
  box.dataset.id = id;
  const fc = await api('/api/fund/check?id=' + id + (probe ? '&probe=1' : ''));
  box.replaceChildren();
  if (!compact) {
    box.append(el('div', '', fc.note || '(설명 없음)'), el('div', 'muted', `게시판 ${fc.board} · 만든 사람 ${short(fc.creator)} · 크기 ${fc.fund.size} 바이트`));
    box.append(renderFund(fc.id, fc.fund, fc.note));
  }
  const ok = fc.samples_verified === fc.samples_asked;
  box.append(el('div', ok ? 'ok' : 'warn', `데이터 표본 확인 ${fc.samples_verified}/${fc.samples_asked}` + (ok ? ' — 요청의 데이터와 일치해요' : ' — 네트워크에서 받는 중이에요. 끝내 안 오면 데이터가 없을 수 있어요')));
  if (fc.preview && fc.media !== 3) { const u = dataURL(fc.preview); if (u) { const img = el('img'); img.src = u; img.alt = '앞부분 미리보기'; box.append(img); } }
  if (!ok) setTimeout(() => { if (box.dataset.id === id && box.isConnected) showFundCheck(id, box, false, compact).catch(() => {}); }, 2000);
}

const openChecks = new Set();
async function refreshFunds() {
  const list = (await api('/api/funds?limit=50')) || [];
  const box = $('#fundList'); box.replaceChildren(); box.className = 'small';
  for (const it of list) {
    const d = el('div', 'item');
    d.append(el('div', 'text', it.note || '(설명 없음)'));
    const f = it.fund;
    d.append(renderFund(it.id, f, it.note));
    const m = el('div', 'meta');
    m.append(el('span', '', `마감 ${timeLeft(it.blocks_left)} 뒤`), el('span', '', `${mediaNames[it.media] || '데이터'} ${Math.ceil(f.size / 1024)}KB`),
      el('span', '', `게시판 ${it.board || '-'}`), el('span', '', f.mine ? '내 요청' : '만든 사람 ' + short(it.creator)));
    if (!it.listed) m.append(el('span', 'warn', '다른 노드에 알리는 중 (작업증명 계산)'));
    const cbox = el('div');
    m.append(act('데이터 확인', () => { openChecks.add(it.id); showFundCheck(it.id, cbox, true, true).catch(fail); }));
    d.append(m, cbox);
    if (openChecks.has(it.id)) showFundCheck(it.id, cbox, false, true).catch(() => {});
    box.append(d);
  }
  empty(box, '지금 후원을 기다리는 요청이 없어요.');
}
$('#fundNewForm').onsubmit = async (e) => {
  e.preventDefault();
  const f = e.target; const fd = new FormData();
  fd.append('file', f.file.files[0]); fd.append('board', f.board.value.trim()); fd.append('note', f.note.value);
  fd.append('tier', f.tier.value); fd.append('days', f.days.value);
  try {
    const r = await api('/api/fund/new', { method: 'POST', body: fd });
    const c = $('#fundCode'); c.hidden = false; c.value = r.code; c.select();
    toast(`요청을 만들었어요. 작업증명을 마치면 모든 노드의 목록에 올라가요 (마감 높이 ${r.deadline}).`);
    refreshFunds();
  } catch (err) { fail(err); }
};
$('#fundCheckForm').onsubmit = async (e) => {
  e.preventDefault();
  try { const fc = await post('/api/fund/check', { code: e.target.code.value }); await showFundCheck(fc.id); } catch (err) { fail(err); }
};

// ---------- bounties ----------
const bountyStates = { open: '모집 중', accepted: '제안 수락됨 · 공개 대기', refunding: '돌려주는 중' };
const openOffers = new Set();
const openPreviews = new Set();
async function refreshBounties() {
  const list = (await api('/api/bounties?limit=100')) || [];
  const box = $('#bountyList'); box.replaceChildren();
  for (const b of list) box.append(renderBounty(b));
  empty(box, '지금 걸린 현상금이 없어요.');
}
function renderBounty(b) {
  const d = el('div', 'item');
  d.append(el('span', 'badge' + (b.status === 'open' ? ' live' : ''), bountyStates[b.status] || b.status), ' ');
  d.append(el('span', 'strong', `현상금 ${num(b.pot)} 코인`), ' ', el('span', 'muted', `· 후원 ${b.backers}명`));
  d.append(el('div', 'text', b.note || '(설명을 불러오는 중)'));
  (b.refs || []).forEach((a) => { const img = el('img'); img.src = '/api/data/' + a; img.alt = '참고'; img.loading = 'lazy';
    img.onerror = () => img.replaceWith(el('div', 'small muted', '참고 이미지를 아직 받지 못했어요')); d.append(img); });
  const m = el('div', 'meta');
  if (b.status === 'open') m.append(el('span', '', `마감 ${timeLeft(b.blocks_left)} 뒤`));
  if (b.failed) m.append(el('span', '', `공개하지 않은 수락 ${b.failed}번 · 마감 ${timeLeft(b.extended)} 늘어남`));
  m.append(el('span', '', `결과 ${tierName(b.tier)} 보관 (지금 1MB에 ${num(b.storage_per_mb)} 코인)`), el('span', '', `게시판 ${b.board || '-'}`),
    el('span', '', b.mine ? '내 현상금' : '요청자 ' + short(b.requester)));
  if (b.status === 'open') {
    const q = S.queued.get(b.id);
    m.append(act(q ? `현상금 올리기 ${num(q.amount)} 코인 대기 · 바꾸기` : '좋아요 · 현상금 올리기', () => openLike({ kind: 'bounty', id: b.id, text: b.note })));
  }
  const sub = el('div', 'sub');
  if (b.status === 'open') {
    m.append(act(`제안 ${b.offers}개 ${openOffers.has(b.id) ? '접기' : '보기'}`, () => { openOffers.has(b.id) ? openOffers.delete(b.id) : openOffers.add(b.id); refreshBounties().catch(fail); }));
    if (!b.mine) m.append(act('결과 제안하기', () => { sub.prepend(offerForm(b)); }));
    if (openOffers.has(b.id)) showOffers(b, sub).catch(fail);
  }
  if (b.status === 'accepted') {
    m.append(el('span', '', `공개 마감 ${timeLeft(b.window_left)} 뒤`));
    if (b.my_offer) sub.append(el('div', 'ok', '내 제안이 수락됐어요. 이 노드가 데이터를 나눠 주고 공개해요. 블록에 담겨 현상금을 받을 때까지 노드를 켜 두세요.'));
    if (b.mine) sub.append(el('div', 'muted', `제안자가 ${timeLeft(b.window_left)} 안에 데이터 전체를 공개해야 해요. 공개되면 현상금은 바로 제안자에게 가요. 못 하면 잠긴 시간만큼 마감을 늘려 다시 모집해요.`));
  }
  d.append(m, sub);
  return d;
}
function offerForm(b) {
  const f = el('form');
  f.innerHTML = '<div class="small">결과 파일 <input type="file" name="file" accept="image/png,image/jpeg,image/gif" required></div>' +
    '<div class="small">무료 미리보기 (선택: 흐리게 하거나 일부만 보여 주는 이미지) <input type="file" name="preview" accept="image/png,image/jpeg,image/gif"></div>' +
    '<textarea name="note" maxlength="300" placeholder="제안 설명 (선택)"></textarea><button>제안 보내기</button>' +
    '<p class="small muted">결과 파일은 수락될 때까지 이 노드에만 있어요. 요청자는 무료 미리보기와 파일 앞부분(진짜 데이터의 일부로 증명됨)만 볼 수 있어요. 제안은 작업증명(몇 초~수십 초)을 마치면 다른 노드에 알려져요. 코인은 필요 없어요.</p>';
  f.onsubmit = async (e) => {
    e.preventDefault();
    const fd = new FormData(); fd.append('bounty', b.id); fd.append('file', f.file.files[0]); fd.append('note', f.note.value);
    if (f.preview.files.length) fd.append('preview', f.preview.files[0]);
    try { await api('/api/bounty/offer', { method: 'POST', body: fd }); toast('제안을 만들었어요. 작업증명을 마치면 요청자에게 보여요.'); f.replaceWith(el('div', 'ok', '제안을 보냈어요.')); } catch (err) { fail(err); }
  };
  return f;
}
async function showOffers(b, box) {
  const offers = await api('/api/bounty/offers?id=' + b.id);
  for (const o of offers) {
    const d = el('div', 'item');
    d.append(el('div', 'text', o.note || '(설명 없음)'));
    const m = el('div', 'meta');
    m.append(el('span', '', `${mediaNames[o.media] || '데이터'} ${Math.ceil(o.size / 1024)}KB`), el('span', '', o.mine ? '내 제안' : '제안자 ' + short(o.hunter)));
    if (!o.listed) m.append(el('span', 'warn', '작업증명 계산 중'));
    const pv = el('div');
    m.append(act('미리보기', () => { openPreviews.add(o.id); preview(o, pv).catch(fail); }));
    if (openPreviews.has(o.id)) preview(o, pv).catch(() => {});
    if (b.mine) m.append(act('이 제안 수락', () => send('/api/bounty/accept', { offer: o.id }).then(() => toast('수락을 보냈어요. 현상금이 잠기고 제안자가 하루 안에 공개해야 해요.')).catch(fail)));
    d.append(m, pv);
    box.append(d);
  }
  empty(box, '아직 제안이 없어요.');
}
async function preview(o, box) {
  box.dataset.id = o.id;
  const p = await api('/api/bounty/preview?offer=' + o.id);
  box.replaceChildren();
  if (p.free) { const u = dataURL(p.free); box.append(el('div', 'small muted', '무료 미리보기 (제안자가 고른 그림, 데이터와 같다는 보장은 없어요)')); if (u) { const i = el('img'); i.src = u; box.append(i); } }
  box.append(el('div', 'small ' + (p.bound_got === p.bound_chunks ? 'ok' : 'muted'), `파일 앞부분 ${p.bound_got}/${p.bound_chunks}조각 (진짜 데이터와 일치 확인)`));
  if (p.bound) { const u = dataURL(p.bound); if (u) { const i = el('img'); i.src = u; i.alt = '앞부분'; box.append(i); } }
  if (p.bound_got < p.bound_chunks) setTimeout(() => { if (box.dataset.id === o.id && box.isConnected) preview(o, box).catch(() => {}); }, 2000);
}
$('#bountyNewForm').onsubmit = async (e) => {
  e.preventDefault();
  const f = e.target;
  try {
    if (f.refs.files.length > 4) return toast('참고 이미지는 4개까지예요', true);
    const refs = [];
    for (const file of f.refs.files) refs.push((await upload(file, '1y')).root);
    await send('/api/bounty/new', { board: f.board.value.trim(), note: f.note.value, amount: f.amount.value.trim(), days: Number(f.days.value), tier: f.tier.value, refs });
    f.reset(); toast('현상금을 걸었어요. 블록에 담기면 목록에 보여요.');
  } catch (err) { fail(err); }
};

// ---------- wallet ----------
async function refreshWallet() {
  const w = await api('/api/wallet');
  const burst = `${w.burst_score.toFixed(1)} / ${w.burst_free}` + (w.burst_next_factor > 1 ? ` · 다음 전송 기본 수수료 ×${w.burst_next_factor}` : '');
  kv($('#wallet'), [['주소', w.address], ['잔액', w.balance + ' 코인'], ['잠긴 보상', w.vesting], ['예치', w.stake],
    ['숙성 중 예치', w.stake_matures_at ? `${w.stake_pending} (높이 ${w.stake_matures_at}에 숙성)` : w.stake_pending], ['보내는 중', w.queued], ['연속 전송', burst]]);
  $('#wallet').firstChild.lastChild.className = 'mono';
  await Promise.all([refreshRLN(), refreshLikeQueue(), refreshTips()]);
}

const rlnNames = { none: '등록 안 됨', pending: '등록 대기', active: '활성', leaving: '해제 중', gone: '종료됨' };
async function refreshRLN() {
  const r = S.rln;
  S.quote = await api('/api/rln/quote');
  for (const p of $$('.per')) p.textContent = epochName();
  const rows = [['상태', rlnNames[r.status] || r.status]];
  if (r.member) {
    rows.push(['내 보증금', `${num(r.bond)} 코인 → ${epochName()} ${r.limit}개`]);
    rows.push(['이번 시간대', `${Math.min(r.limit, r.cap)}개 중 ${r.remaining}개 남음 · ${r.epoch_minutes_left}분 뒤 충전`]);
  }
  if (r.status === 'leaving') rows.push(['보증금 돌아오는 높이', r.release]);
  rows.push(['회원 수', r.members]);
  if (r.pseudonym) rows.push(['가명', short(r.pseudonym)]);
  kv($('#rln'), rows);
  $('#rlnForeign').hidden = !r.foreign_use;
  $('#rlnLeave').hidden = r.status !== 'active';
  $('#rlnJoin').hidden = r.member;
  const sel = $('#calcLimit');
  if (sel.options.length !== S.quote.bond_for.length) {
    sel.replaceChildren(...S.quote.bond_for.map((_, i) => new Option(String(i + 1), String(i + 1))));
    sel.value = '4';
  }
  calcFromLimit();
  if ($('#calcAmount').value) calcFromAmount();
}
function calcFromLimit() {
  const l = Number($('#calcLimit').value || 1); const b = S.quote.bond_for[l - 1];
  $('#calcBond').textContent = `보증금 ${num(b)} 코인이 필요해요`;
}
let calcTimer = 0;
function calcFromAmount() {
  clearTimeout(calcTimer);
  calcTimer = setTimeout(async () => {
    const a = $('#calcAmount').value.trim();
    if (!a) { $('#calcCount').textContent = ''; return; }
    try {
      const q = await api('/api/rln/quote?amount=' + encodeURIComponent(a));
      $('#calcCount').textContent = q.limit ? `${epochName()} ${q.limit}개` + (q.next_bond ? ` (${num(q.next_bond)} 코인이면 ${q.limit + 1}개)` : ' (최대)')
        : `모자라요: 1개에 ${num(q.bond_for[0])} 코인`;
    } catch (e) { $('#calcCount').textContent = e.message; }
  }, 250);
}
$('#calcAmount').oninput = calcFromAmount;
$('#calcLimit').onchange = () => { calcFromLimit(); $('#calcAmount').value = num(S.quote.bond_for[Number($('#calcLimit').value) - 1]); calcFromAmount(); };
$('#rlnJoinBtn').onclick = () => {
  const a = $('#calcAmount').value.trim() || num(S.quote.bond_for[Number($('#calcLimit').value) - 1]);
  if (!confirm(`예치금에서 ${a} 코인을 보증금으로 걸까요?`)) return;
  post('/api/rln/join', { amount: a }).then(() => toast('익명 글쓰기 등록을 보냈어요')).catch(fail);
};
$('#rlnLeave').onclick = () => { if (confirm('등록을 해제할까요? 보증금은 해제 기간이 지난 뒤 돌아와요.'))
  post('/api/rln/leave', {}).then(() => toast('등록 해제를 보냈어요')).catch(fail); };
$('#rlnAck').onclick = () => post('/api/rln/ack', {}).then(() => toast('이 기기에서 익명 글쓰기를 다시 시작해요')).catch(fail);

async function refreshLikeQueue() {
  const lq = S.likes || { likes: [] };
  const box = $('#likeQueue'); box.replaceChildren();
  const kinds = { fund: '펀딩 후원', bounty: '현상금' };
  for (const l of lq.likes || []) {
    const d = el('div', 'row');
    d.append(el('span', '', `${kinds[l.fund ? 'fund' : l.bounty ? 'bounty' : ''] || '좋아요'} ${num(l.amount)} 코인 · ${short(l.id)}`), act('빼기', () => unqueue(l.id)));
    box.append(d);
  }
  if ((lq.likes || []).length) {
    const d = el('div', 'row');
    d.append(el('span', 'strong', `${lq.likes.length}개 · ${num(lq.coins)} 코인 · 지금 보내면 수수료 약 ${num(lq.fee)} 코인`),
      el('button', '', '지금 보내기'));
    d.lastChild.onclick = () => send('/api/likes', {}).then((r) => { toast(`좋아요 ${r.sent}개를 한 번에 보냈어요`); tick(); }).catch(fail);
    box.append(d);
    if (lq.expiring_soon) box.append(el('div', 'warn', `${lq.expiring_soon}개는 하루 안에 좋아요 집계 기간(${lq.window_days}일)이 끝나요. 그 전에 보내 주세요.`));
  }
  empty(box, '대기 중인 좋아요가 없어요.');
  box.append(el('div', 'muted', '자동 보내기: ' + autoText(lq.auto)));
}
function autoText(a = {}) {
  const parts = [];
  if (a.minutes) parts.push(a.minutes >= 1440 ? '하루' : a.minutes >= 60 ? (a.minutes / 60) + '시간' : a.minutes + '분');
  if (a.count) parts.push(a.count + '개');
  return parts.length ? parts.join(' 또는 ') + ' 되면 보내요' : '꺼짐';
}

async function refreshTips() {
  const accts = await api('/api/tips');
  const box = $('#tipAccounts'); box.replaceChildren();
  for (const a of accts) {
    const d = el('div', 'item');
    d.append(el('div', '', `${a.pseudonym ? '가명' : '익명 글'} ${short(a.address)} · ${num(a.balance)} 코인`));
    const f = el('form', 'row');
    f.innerHTML = '<input name="to" placeholder="받는 주소 (비우면 내 지갑)"><input name="amount" placeholder="코인" class="amt" required><button>보내기</button>';
    f.onsubmit = (e) => { e.preventDefault();
      send('/api/tips', { from: a.address, to: f.to.value.trim(), amount: f.amount.value.trim() }).then(() => { toast('보냈어요'); refreshTips(); }).catch(fail); };
    if (Number(a.balance) > 0) d.append(f);
    box.append(d);
  }
  empty(box, '받은 후원이 없어요.');
}
$('#sendForm').onsubmit = (e) => { e.preventDefault();
  send('/api/transfer', { to: e.target.to.value.trim(), amount: e.target.amount.value }).then(() => { e.target.reset(); toast('송금을 보냈어요'); }).catch(fail); };
$('#stakeForm').onsubmit = (e) => { e.preventDefault(); const op = e.submitter.dataset.op;
  send('/api/' + op, { amount: e.target.amount.value }).then(() => toast(op === 'stake' ? '예치를 보냈어요' : '예치 해제를 보냈어요')).catch(fail); };

// ---------- activity ----------
async function refreshActivity() {
  const a = await api('/api/activity');
  const fill = (sel, posts, text) => { const box = $(sel); box.replaceChildren(); posts.forEach((p) => box.append(renderPost(p))); empty(box, text); };
  fill('#actPending', a.pending, '보내는 중인 글이 없어요.');
  fill('#actPosts', a.posts, '아직 쓴 글이 없어요.');
  fill('#actLiked', a.liked, '좋아요한 글이 없어요.');
  const fb = $('#actFunds'); fb.replaceChildren();
  for (const it of a.funds) { const d = el('div', 'item'); d.append(el('div', 'text', it.note || '(설명 없음)'), renderFund(it.id, it.fund, it.note), el('div', 'meta', `마감 ${timeLeft(it.blocks_left)} 뒤`)); fb.append(d); }
  empty(fb, '진행 중인 내 크라우드 펀딩이 없어요. 지난 요청은 "내 글"에 있어요.');
  const bb = $('#actBounties'); bb.replaceChildren();
  a.bounties.forEach((b) => bb.append(renderBounty(b)));
  empty(bb, '진행 중인 내 현상금이나 수락된 제안이 없어요.');
  const ob = $('#actOffers'); ob.replaceChildren();
  for (const o of a.offers) {
    const d = el('div', 'item');
    d.append(el('div', 'text', o.note || '(설명 없음)'));
    d.append(el('div', 'meta', [`현상금 ${short(o.bounty)}`, `${Math.ceil(o.size / 1024)}KB`, o.closed ? '마감됨' : o.listed ? '알려짐' : '작업증명 계산 중'].join(' · ')));
    ob.append(d);
  }
  empty(ob, '보낸 제안이 없어요.');
}

// ---------- settings ----------
async function refreshSettings() {
  const s = S.status;
  const pv = s.privacy || {}; const nat = s.nat || {}; const cfg = s.settings || {};
  const rows = [];
  if (pv.onion) rows.push(['Tor 주소', pv.onion]);
  rows.push(['나가는 연결', pv.proxy ? (pv.proxy_only ? '프록시로만 (직접 연결 안 함)' : 'SOCKS5 프록시 경유') : '직접']);
  if (nat.external) rows.push(['외부 주소', nat.external]);
  if (nat.status) rows.push(['포트', nat.status]);
  rows.push(['커버 트래픽', pv.cover ? `켜짐 · 업로드 약 ${(pv.cover_bytes_per_second / 1024).toFixed(1)} KB/s (한 달 약 ${pv.cover_gb_per_month.toFixed(1)} GB)` : '꺼짐']);
  rows.push(['보내기 지연', cfg.send_delay_seconds ? `0~${Math.round(cfg.send_delay_seconds / 60)}분 무작위` : '없음']);
  rows.push(['피어', s.peers], ['P2P 주소', s.p2p_listen || '받지 않음']);
  kv($('#netInfo'), rows);
  const m = s.mining;
  kv($('#mining'), [['채굴', cfg.mine ? `켜짐 · 디스크 ${cfg.pledge_gb.toFixed(0)} GB` : '꺼짐'], ['파티션', m.assigned_partitions], ['패킹한 조각', m.packed_chunks],
    ['VDF 단계', m.vdf_steps], ['찾은 블록', m.blocks_found], ['최근', m.last_result || '-']]);
  const keyText = { locked: ['ok', '키 파일이 모두 비밀번호로 잠겨 있어요.'],
    partial: ['warn', '일부 키 파일만 잠겨 있어요. 비밀번호를 다시 정하면 모두 잠가요.'],
    plain: ['warn', '키 파일이 잠겨 있지 않아요. 이 컴퓨터를 쓰는 사람은 누구나 지갑을 쓸 수 있어요.'] }[cfg.keys] || ['muted', ''];
  $('#pwInfo').replaceChildren(el('div', keyText[0], keyText[1]));
  const locked = cfg.keys !== 'plain';
  $('#pwCurrentRow').hidden = !locked;
  $('#pwSet').textContent = locked ? '비밀번호 바꾸기' : '잠그기';
  $('#pwClear').hidden = !locked;
  $('#autoSummary').textContent = '지금: ' + autoText((S.likes || {}).auto);
  const af = $('#autoForm'); const a = (S.likes || {}).auto || {};
  if (!af.contains(document.activeElement)) { af.minutes.value = String(a.minutes || 0); af.count.value = a.count || 0; }
}
$('#pwForm').onsubmit = async (e) => {
  e.preventDefault();
  const f = e.target; const op = e.submitter && e.submitter.dataset.op;
  const next = op === 'clear' ? '' : f.next.value;
  if (op === 'set' && (!next || next !== f.again.value)) return toast('새 비밀번호를 두 번 똑같이 입력하세요', true);
  if (op === 'clear' && !confirm('잠금을 풀까요? 이 컴퓨터를 쓰는 사람은 누구나 지갑을 쓸 수 있게 돼요.')) return;
  try {
    await post('/api/password', { current: f.current.value, new: next });
    f.reset(); toast(next ? '키를 새 비밀번호로 잠갔어요' : '키 잠금을 풀었어요');
    await tick();
  } catch (err) { fail(err.status === 403 ? new Error('지금 비밀번호가 맞지 않아요') : err); }
};
$('#autoForm').onsubmit = (e) => { e.preventDefault();
  post('/api/likes', { auto: { minutes: Number(e.target.minutes.value), count: Number(e.target.count.value) } })
    .then(() => { toast('자동 보내기 설정을 바꿨어요'); tick(); }).catch(fail); };

// ---------- loop ----------
// A page is not redrawn while one of its drawn-in forms (a comment, an
// offer, a send from a tip address) is being filled in.
function busy() {
  for (const f of $$('.page:not([hidden]) .item form, .page:not([hidden]) .sub form, .page:not([hidden]) #thread form')) {
    if (f.contains(document.activeElement)) return true;
    for (const i of f.querySelectorAll('input, textarea')) if (i.value || (i.files && i.files.length)) return true;
  }
  return $('#likeDlg').open;
}
async function tick() {
  try { await refreshStatus(); } catch (e) { toast('노드에 연결할 수 없어요: ' + e.message, true); return; }
  if (!busy()) await refreshPage().catch(() => {});
}
showPage(location.hash.slice(1) || 'board');
tick(); setInterval(tick, 5000);
