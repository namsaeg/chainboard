'use strict';
const $ = (s) => document.querySelector(s);
let board = '';
let openThread = '';
// Waiting likes by "id:anon", the smallest tip or pledge, whether this node
// is an active anonymous member, and the block time (for deadlines).
const likeState = { queued: new Map(), min: '0', member: false, blockSec: 60 };
const qkey = (id, anon) => id + ':' + (anon ? 1 : 0);

async function api(path, opts = {}) {
  opts.headers = Object.assign({ 'X-Chainboard': '1' }, opts.headers || {});
  const r = await fetch(path, opts);
  const j = await r.json().catch(() => ({}));
  if (!r.ok) { const e = new Error(j.error || r.statusText); e.status = r.status; e.body = j; throw e; }
  return j;
}
const post = (path, body) => api(path, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body) });

// The node refuses a surcharged send, or the last anonymous post of an epoch,
// with 409 and the warnings; the user must accept them before it is sent.
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
const tierNames = { 1: '1개월', 65535: '영구' };
function tierName(t) { return tierNames[t] || (t % 12 === 0 ? (t / 12) + '년' : t + '개월'); }

function coin(units) { return (Number(units) / 1e8).toFixed(4).replace(/\.?0+$/, ''); }
function el(tag, cls, text) { const e = document.createElement(tag); if (cls) e.className = cls; if (text !== undefined) e.textContent = text; return e; }
function short(s) { return s ? s.slice(0, 10) + '…' : ''; }
function when(t) { return t ? new Date(t * 1000).toLocaleString() : '미확정'; }
function toast(msg, bad) { const s = $('#status'); s.textContent = msg; s.className = bad ? 'err' : 'muted'; }

async function refreshStatus() {
  try {
    const s = await api('/api/status');
    $('#ver').textContent = `v${s.version} · ${s.network}`;
    likeState.blockSec = s.block_seconds || 60;
    const ni = $('#netInfo'); ni.replaceChildren();
    const pv = s.privacy || {}; const nat = s.nat || {};
    const netRows = [];
    if (pv.onion) netRows.push(['Tor 주소', pv.onion]);
    netRows.push(['나가는 연결', pv.proxy ? (pv.proxy_only ? '프록시로만 (직접 연결 안 함)' : 'SOCKS5 프록시 경유') : '직접']);
    if (nat.external) netRows.push(['외부 주소', nat.external]);
    if (nat.status) netRows.push(['포트', nat.status]);
    netRows.push(['커버 트래픽', pv.cover ? `켜짐 · 업로드 약 ${(pv.cover_bytes_per_second / 1024).toFixed(1)} KB/s (한 달 약 ${pv.cover_gb_per_month.toFixed(1)} GB)` : '꺼짐']);
    for (const [k, v] of netRows) { const d = el('div'); d.append(el('span', 'muted', k + ' '), el('span', '', String(v))); ni.append(d); }
    $('#status').className = 'muted';
    $('#status').textContent = `높이 ${s.height} · 피어 ${s.peers} · 저장 가격 ${s.storage_price} · 기본 수수료 ${s.base_fee_per_byte}/byte · 대기 ${s.mempool}`;
    const m = s.mining;
    $('#mining').textContent = `파티션 ${m.assigned_partitions} · 패킹 ${m.packed_chunks} · VDF ${m.vdf_steps} · 찾은 블록 ${m.blocks_found} ${m.last_result || ''}`;
    const w = await api('/api/wallet');
    const box = $('#wallet'); box.replaceChildren();
    const burst = `${w.burst_score.toFixed(1)} / ${w.burst_free}` + (w.burst_next_factor > 1 ? ` · 다음 전송 기본 수수료 ×${w.burst_next_factor}` : '');
    for (const [k, v] of [['주소', w.address], ['잔액', w.balance], ['잠긴 보상', w.vesting], ['예치', w.stake], ['숙성 중 예치', w.stake_matures_at ? `${w.stake_pending} (높이 ${w.stake_matures_at}에 숙성)` : w.stake_pending], ['보내는 중', w.queued], ['연속 전송', burst]]) {
      const d = el('div'); d.append(el('span', 'muted', k + ' '), el('span', '', String(v))); box.append(d);
    }
    await refreshLikeQueue();
    const r = await api('/api/rln');
    likeState.member = r.status === 'active';
    $('#pseudoBox').hidden = !r.pseudonym;
    if (r.pseudonym) $('#pseudoBal').textContent = r.pseudonym_balance || '0';
    const names = { none: '등록 안 됨', pending: '등록 대기', active: '활성', leaving: '해제 중', gone: '종료됨' };
    const rb = $('#rln'); rb.replaceChildren();
    const rows = [['상태', names[r.status] || r.status], ['회원 수', r.members], ['지금 보증금', `글 1개당 ${r.bond_per_message} · ${r.max_limit}개 ${r.bond_full} 코인`]];
    if (r.member) rows.push(['보증금', r.bond], ['이번 에포크 남은 익명 메시지', `${r.remaining} / ${Math.min(r.limit, r.cap)}개 (글 1개 또는 좋아요 묶음 1개) · 약 ${r.epoch_minutes_left}분 뒤 충전`]);
    if (r.status === 'leaving') rows.push(['보증금 반환 높이', r.release]);
    if (r.pseudonym) rows.push(['가명', short(r.pseudonym)]);
    for (const [k, v] of rows) { const d = el('div'); d.append(el('span', 'muted', k + ' '), el('span', '', String(v))); rb.append(d); }
    if (r.member && r.remaining === 1) rb.append(el('div', 'warn', '다음 익명 글이나 익명 좋아요 묶음이 이번 에포크의 마지막이에요.'));
    if (r.member && r.remaining === 0) rb.append(el('div', 'warn', `이번 에포크 한도를 다 썼어요. 약 ${r.epoch_minutes_left}분 뒤에 다시 쓸 수 있어요.`));
    $('#rlnForeign').hidden = !r.foreign_use;
    $('#rlnLeave').hidden = r.status !== 'active';
    $('#rlnJoinForm').hidden = r.member;
  } catch (e) { toast('노드에 연결할 수 없어요: ' + e.message, true); }
}

async function refreshBoards() {
  const bs = await api('/api/boards');
  const ul = $('#boards'); ul.replaceChildren();
  Object.entries(bs).sort((a, b) => b[1] - a[1]).forEach(([name, n]) => {
    const li = el('li', '', `${name} (${n})`); li.onclick = () => selectBoard(name); ul.append(li);
  });
}

function renderPost(p, isComment) {
  const d = el('div', 'post' + (p.pending ? ' pending' : ''));
  if (p.text_state === 'expired') d.append(el('div', 'text muted', '보존 기간이 끝나 본문이 지워진 글이에요.'));
  else if (p.text_state === 'loading') d.append(el('div', 'text muted', '본문을 불러오는 중…'));
  else d.append(el('div', 'text', p.text));
  (p.attach || []).forEach((a) => { const img = el('img'); img.src = '/api/data/' + a; img.alt = '첨부'; img.loading = 'lazy';
    img.onerror = () => img.replaceWith(el('div', 'small muted', '첨부를 이 노드가 아직 갖고 있지 않아요 · ' + short(a))); d.append(img); });
  const m = el('div', 'meta');
  const who = p.lane === 'anon' ? (p.pseudonym ? '가명 ' + short(p.author) : '익명') : short(p.author);
  m.append(el('span', '', who), el('span', '', when(p.time)), el('span', '', p.lane === 'anon' ? '익명 차선' : '수수료 차선'));
  if (p.tier) m.append(el('span', '', '보관 ' + tierName(p.tier) + (p.until ? ` (높이 ${p.until}까지)` : '')));
  if (p.fund) d.append(renderFund(p.id, p.fund));
  if (p.pending) m.append(el('span', '', '전송 대기/미확정'));
  else {
    if (p.lane !== 'fund') {
      const mine = p.liked || p.liked_anon ? ' ✓' : '';
      const like = el('a', '', `좋아요 ${p.likes}${mine}` + (Number(p.tips) > 0 ? ` · 후원 ${coin(p.tips)} 코인` : ''));
      like.onclick = () => openLike({ kind: 'post', post: p });
      m.append(like);
      for (const anon of [false, true]) {
        const q = likeState.queued.get(qkey(p.id, anon));
        if (!q) continue;
        const undo = el('a', '', (anon ? '익명 좋아요' : q.amount ? `후원 ${q.amount} 코인` : '좋아요') + ' 대기 · 취소');
        undo.onclick = () => post('/api/like', { id: p.id, anon, undo: true }).then(() => { toast('대기 중인 좋아요를 뺐어요'); tick(); }).catch((e) => toast(e.message, true));
        m.append(undo);
      }
    }
    if (p.tier && p.tier !== 65535 && p.text_state === 'ok') { const x = el('a', '', '글 연장'); x.onclick = () => {
      const tier = prompt('글을 보관할 기간 (1y, 5y, 20y, perm) — 누구나 연장할 수 있어요', '1y');
      if (tier) send('/api/extend', { post: p.id, tier }).then(() => toast('연장을 보냈어요')).catch((e) => toast(e.message, true)); }; m.append(x); }
    if (!isComment) { const c = el('a', '', `댓글 ${p.comments}`); c.onclick = () => showThread(p.id); m.append(c); }
    (p.attach || []).forEach((a) => { const x = el('a', '', '첨부 연장'); x.onclick = () => {
      const tier = prompt('연장할 보존 기간 (1y, 5y, 20y, perm)', '5y'); if (tier) send('/api/extend', { root: a, tier }).then(() => toast('연장을 보냈어요')).catch((e) => toast(e.message, true)); }; m.append(x); });
  }
  d.append(m);
  return d;
}

const fundStates = { new: '새 요청 (첫 후원과 함께 체인에 올라가요)', open: '모금 중', uploaded: '업로드됨 · 증명 대기', stored: '저장 완료', closed: '마감 (환불됨)' };
function renderFund(id, f) {
  const box = el('div', 'fund small');
  const line = [fundStates[f.status] || f.status];
  if (f.status === 'open' || f.status === 'uploaded') line.push(`모인 코인 ${f.raised} (후원 ${f.backers}명)`, `마감 높이 ${f.deadline}`);
  if (f.status === 'open' || f.status === 'new') line.push(`최소 ${tierName(f.min_tier)} 비용 지금 ${f.need_for_min} 코인`);
  if (f.tier) line.push((f.status === 'open' ? '지금 올리면 ' : '산 기간 ') + tierName(f.tier));
  box.append(el('div', '', line.join(' · ')));
  if (f.status === 'open' || f.status === 'new') {
    const q = likeState.queued.get(qkey(id, false));
    const b = el('a', '', q ? `후원 ${q.amount} 코인 대기 · 바꾸기` : '좋아요 · 후원');
    b.onclick = () => openLike({ kind: 'fund', id, fund: f });
    box.append(b);
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
    box.append(renderFund(fc.id, fc.fund));
  }
  const ok = fc.samples_verified === fc.samples_asked;
  box.append(el('div', ok ? '' : 'warn', `데이터 표본 확인 ${fc.samples_verified}/${fc.samples_asked}` + (ok ? ' — 요청의 데이터 루트와 일치해요' : ' — 네트워크에서 받는 중이에요. 끝내 안 오면 데이터가 없을 수 있어요')));
  if (fc.preview && fc.media !== 3) { const img = el('img'); img.src = 'data:image/*;base64,' + fc.preview; img.alt = '앞부분 미리보기'; box.append(img); }
  if (!ok) setTimeout(() => { if (box.dataset.id === id && box.isConnected) showFundCheck(id, box, false, compact).catch(() => {}); }, 2000);
}

const mediaNames = { 1: '이미지', 2: '이미지', 3: '영상', 4: '파일' };
function minutesLeft(blocks) {
  const m = Math.round(blocks * likeState.blockSec / 60);
  return m < 90 ? `약 ${m}분` : m < 2880 ? `약 ${Math.round(m / 60)}시간` : `약 ${Math.round(m / 1440)}일`;
}

// The list of crowdfunding in progress (gossiped requests and those on chain).
const openChecks = new Set();
async function refreshFunds() {
  const list = (await api('/api/funds?limit=50')) || [];
  const box = $('#fundList'); box.replaceChildren(); box.className = 'small';
  if (!list.length) { box.append(el('div', 'muted', '지금 후원을 기다리는 요청이 없어요.')); return; }
  for (const it of list) {
    const d = el('div', 'fundItem');
    d.append(el('div', 'text', it.note || '(설명 없음)'));
    const f = it.fund; const raised = Number(f.raised || 0); const need = Number(f.need_for_min || 0);
    const bar = el('div', 'bar'); const fill = el('div'); fill.style.width = Math.min(100, need ? raised / need * 100 : 0) + '%'; bar.append(fill); d.append(bar);
    const m = el('div', 'meta');
    m.append(el('span', '', `${raised} / ${need || '?'} 코인 (최소 ${tierName(f.min_tier)} 저장 비용)`),
      el('span', '', `후원 ${f.backers || 0}명`), el('span', '', `마감 ${minutesLeft(it.blocks_left)} 뒤`),
      el('span', '', `${mediaNames[it.media] || '데이터'} ${Math.ceil(f.size / 1024)}KB`), el('span', '', `게시판 ${it.board || '-'}`),
      el('span', '', f.mine ? '내 요청' : '만든 사람 ' + short(it.creator)));
    if (!it.listed) m.append(el('span', 'warn', '다른 노드에 알리는 중 (작업증명 계산)'));
    const q = likeState.queued.get(qkey(it.id, false));
    const like = el('a', '', q ? `후원 ${q.amount} 코인 대기 · 바꾸기` : '좋아요 · 후원');
    like.onclick = () => openLike({ kind: 'fund', id: it.id, fund: f, note: it.note });
    const chk = el('a', '', '데이터 확인');
    const cbox = el('div');
    chk.onclick = () => { openChecks.add(it.id); showFundCheck(it.id, cbox, true, true).catch((e) => toast(e.message, true)); };
    m.append(like, chk);
    d.append(m, cbox);
    if (openChecks.has(it.id)) showFundCheck(it.id, cbox, false, true).catch(() => {});
    box.append(d);
  }
}

// The like dialog: the same for posts and funding requests. A like may carry
// coins (a tip to the author, or a pledge to a request); it waits in its
// queue until sent ("바로 보내기" sends the queue with it).
let likeCtx = null;
function openLike(ctx) {
  likeCtx = ctx;
  const f = $('#likeForm'); f.reset();
  const fund = ctx.kind === 'fund';
  const p = ctx.post || {};
  $('#likeTitle').textContent = fund ? '크라우드 펀딩 좋아요 · 후원' : '좋아요';
  const info = fund ? (ctx.note || '') : (p.text || '').slice(0, 60);
  $('#likeInfo').textContent = info;
  const tippable = fund || p.tippable;
  f.amount.disabled = !tippable;
  f.amount.required = fund;
  $('#likeMin').textContent = tippable ? `최소 ${likeState.min} 코인` + (fund ? ' · 마감까지 저장되지 않으면 돌아와요' : ' · 0이면 좋아요만') : '';
  $('#likeAnonRow').hidden = fund;
  f.anon.disabled = !likeState.member;
  const notes = [];
  if (!fund && !p.tippable) notes.push('완전 익명 글은 받을 주소가 없어 후원할 수 없어요. 좋아요만 보낼 수 있어요.');
  if (!fund && p.pseudonym) notes.push('가명 글의 후원은 그 가명의 주소로 가요.');
  if (!fund && (p.liked || p.liked_anon)) notes.push('이미 좋아요가 세어진 글이에요. 후원은 더 보낼 수 있어요(좋아요 수는 그대로).');
  if (!fund && !likeState.member) notes.push('익명 좋아요는 익명 글쓰기에 등록해야 쓸 수 있어요.');
  $('#likeNote').textContent = notes.join(' ');
  f.anon.onchange = () => { f.amount.disabled = f.anon.checked || !tippable; if (f.anon.checked) f.amount.value = ''; };
  $('#likeDlg').showModal();
}
$('#likeDlg').onclose = async () => {
  const how = $('#likeDlg').returnValue; const f = $('#likeForm'); const ctx = likeCtx;
  if (!ctx || (how !== 'queue' && how !== 'now')) return;
  const body = { id: ctx.kind === 'fund' ? ctx.id : ctx.post.id, amount: f.amount.value.trim(), fund: ctx.kind === 'fund', anon: f.anon.checked && ctx.kind !== 'fund' };
  try {
    if (how === 'now') { await send('/api/like', Object.assign({ now: true }, body)); toast(body.anon ? '익명 좋아요를 보냈어요 (증명에 몇 초 걸려요)' : '좋아요를 보냈어요'); }
    else { const r = await post('/api/like', body); toast(`담았어요 · ${r.waiting}개 대기 중. 보낼 때 지갑의 "지금 보내기"를 누르세요`); }
  } catch (e) { toast(e.message, true); }
  tick();
};

// Both queues, with what sending each now would cost.
async function refreshLikeQueue() {
  const lq = await api('/api/likes');
  likeState.min = lq.min_amount;
  likeState.queued = new Map();
  const box = $('#likeQueue'); box.replaceChildren();
  for (const [anon, q] of [[false, lq.signed], [true, lq.anon]]) {
    (q.likes || []).forEach((l) => likeState.queued.set(qkey(l.id, anon), l));
    if (!q.likes || !q.likes.length) continue;
    const d = el('div', 'queueRow');
    const text = anon ? `익명 좋아요 대기 ${q.likes.length}개 (한 번에 ${q.next}개, 익명 메시지 1개 사용)`
      : `좋아요 대기 ${q.likes.length}개` + (Number(q.coins) > 0 ? ` · 후원 ${q.coins} 코인` : '') + ` · 지금 보내면 수수료 약 ${q.fee} 코인`;
    const go = el('a', '', '지금 보내기');
    go.onclick = () => send('/api/likes', { anon }).then((r) => { toast(`좋아요 ${r.sent}개를 한 번에 보냈어요`); tick(); }).catch((e) => toast(e.message, true));
    d.append(el('span', '', text), go);
    if (q.expiring_soon) d.append(el('div', 'warn', `${q.expiring_soon}개는 하루 안에 좋아요 집계 기간(${lq.window_days}일)이 끝나요. 그 전에 보내 주세요.`));
    box.append(d);
  }
  const a = lq.auto || {};
  const parts = [];
  if (a.minutes) parts.push(a.minutes >= 1440 ? '하루' : a.minutes >= 60 ? (a.minutes / 60) + '시간' : a.minutes + '분');
  if (a.count) parts.push(a.count + '개');
  $('#autoSummary').textContent = '좋아요 자동 보내기: ' + (parts.length ? parts.join(' 또는 ') + ' 되면' : '끔');
  const af = $('#autoForm');
  if (document.activeElement && !af.contains(document.activeElement)) { af.minutes.value = String(a.minutes || 0); af.count.value = a.count || 0; }
}

async function refreshPosts() {
  if (!board) return;
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
  const close = el('a', 'small', '닫기'); close.style.cursor = 'pointer'; close.onclick = () => { openThread = ''; box.replaceChildren(); };
  card.append(close, renderPost(t.post));
  (t.comments || []).forEach((c) => { const r = renderPost(c, true); r.style.marginLeft = '16px'; card.append(r); });
  const f = el('form'); const ta = el('textarea'); ta.placeholder = '댓글'; ta.maxLength = 1000; const b = el('button', '', '댓글 쓰기');
  const ln = el('select'); ln.append(new Option('익명', 'anon'), new Option('수수료', 'fee'));
  f.append(ta, ln, b);
  f.onsubmit = (e) => { e.preventDefault(); send('/api/post', { board: t.post.board, text: ta.value, parent: id, lane: ln.value })
    .then(() => { ta.value = ''; toast('댓글을 보냈어요'); }).catch((err) => toast(err.message, true)); };
  card.append(f);
  box.append(card);
}

function selectBoard(name) { board = name; $('#boardTitle').textContent = name; $('#thread').replaceChildren(); refreshPosts(); }

$('#boardForm').onsubmit = (e) => { e.preventDefault(); selectBoard(e.target.name.value.trim()); };
$('#bestOnly').onchange = refreshPosts;
$('#postForm').onsubmit = async (e) => {
  e.preventDefault();
  if (!board) return toast('먼저 게시판을 고르세요', true);
  const f = e.target;
  try {
    const attach = [];
    let note = '';
    if (f.file.files.length) {
      if (f.lane.value === 'anon') return toast('첨부는 수수료 차선에서만 올릴 수 있어요', true);
      const up = (confirmed) => { const fd = new FormData(); fd.append('file', f.file.files[0]); fd.append('tier', f.tier.value);
        if (confirmed) fd.append('confirm', 'true'); return api('/api/upload', { method: 'POST', body: fd }); };
      let r;
      try { r = await up(false); } catch (e) { if (!accepted(e)) return toast('보내지 않았어요', true); r = await up(true); }
      attach.push(r.root);
      if (r.action === 'reuse') note = ' 같은 파일이 이미 저장돼 있어 다시 올리지 않았어요.';
      if (r.action === 'extend') note = ' 같은 파일이 이미 저장돼 있어 보존 기간만 늘렸어요.';
    }
    await send('/api/post', { board, text: f.text.value, lane: f.lane.value, pseudonym: f.pseudonym.checked, attach, tier: f.textTier.value });
    f.reset(); toast('글을 보냈어요. 잠시 후 미확정 글로 보여요.' + note); refreshPosts();
  } catch (err) { toast(err.message, true); }
};
$('#stakeForm').onsubmit = (e) => { e.preventDefault(); const op = e.submitter.dataset.op;
  send('/api/' + op, { amount: e.target.amount.value }).then(() => toast(op === 'stake' ? '예치를 보냈어요' : '예치 해제를 보냈어요')).catch((err) => toast(err.message, true)); };
$('#rlnJoinForm').onsubmit = (e) => { e.preventDefault();
  post('/api/rln/join', { amount: e.target.amount.value }).then(() => toast('익명 글쓰기 등록을 보냈어요')).catch((err) => toast(err.message, true)); };
$('#rlnLeave').onclick = () => { if (confirm('등록을 해제할까요? 보증금은 해제 기간이 지난 뒤 돌아와요.'))
  post('/api/rln/leave', {}).then(() => toast('등록 해제를 보냈어요')).catch((err) => toast(err.message, true)); };
$('#sendForm').onsubmit = (e) => { e.preventDefault();
  send('/api/transfer', { to: e.target.to.value.trim(), amount: e.target.amount.value }).then(() => toast('송금을 보냈어요')).catch((err) => toast(err.message, true)); };

$('#fundNewForm').onsubmit = async (e) => {
  e.preventDefault();
  const f = e.target; const fd = new FormData();
  fd.append('file', f.file.files[0]); fd.append('board', f.board.value.trim()); fd.append('note', f.note.value);
  fd.append('tier', f.tier.value); fd.append('days', f.days.value);
  try {
    const r = await api('/api/fund/new', { method: 'POST', body: fd });
    const c = $('#fundCode'); c.hidden = false; c.value = r.code; c.select();
    toast(`요청을 만들었어요. 작업증명을 마치면 모든 노드의 펀딩 목록에 올라가요 (마감 높이 ${r.deadline}). 모이면 이 노드가 알아서 올리고 증명해요.`);
  } catch (err) { toast(err.message, true); }
};
$('#fundCheckForm').onsubmit = async (e) => {
  e.preventDefault();
  try {
    const fc = await post('/api/fund/check', { code: e.target.code.value });
    await showFundCheck(fc.id);
  } catch (err) { toast(err.message, true); }
};
$('#rlnAck').onclick = () => post('/api/rln/ack', {}).then(() => toast('이 기기에서 익명 글쓰기를 다시 시작해요')).catch((e) => toast(e.message, true));
const syncTier = () => { const anon = $('#postForm').lane.value === 'anon'; $('#postForm').textTier.disabled = anon;
  if (anon) $('#postForm').textTier.value = '1m'; };
$('#postForm').lane.onchange = syncTier; syncTier();

$('#autoForm').onsubmit = (e) => { e.preventDefault();
  post('/api/likes', { auto: { minutes: Number(e.target.minutes.value), count: Number(e.target.count.value) } })
    .then(() => { toast('자동 보내기 설정을 바꿨어요'); tick(); }).catch((err) => toast(err.message, true)); };
$('#pseudoForm').onsubmit = (e) => { e.preventDefault();
  send('/api/pseudo/send', { to: e.target.to.value.trim(), amount: e.target.amount.value }).then(() => { e.target.reset(); toast('가명 잔액을 보냈어요'); }).catch((err) => toast(err.message, true)); };

async function tick() {
  await refreshStatus();
  await refreshFunds().catch(() => {});
  await refreshBoards().catch(() => {});
  await refreshPosts().catch(() => {});
  if (openThread) showThread(openThread).catch(() => {});
}
tick(); setInterval(tick, 5000);
