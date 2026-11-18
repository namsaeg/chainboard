'use strict';
const $ = (s) => document.querySelector(s);
let board = '';
let openThread = '';

async function api(path, opts = {}) {
  opts.headers = Object.assign({ 'X-Chainboard': '1' }, opts.headers || {});
  const r = await fetch(path, opts);
  const j = await r.json().catch(() => ({}));
  if (!r.ok) throw new Error(j.error || r.statusText);
  return j;
}
const post = (path, body) => api(path, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body) });

function el(tag, cls, text) { const e = document.createElement(tag); if (cls) e.className = cls; if (text !== undefined) e.textContent = text; return e; }
function short(s) { return s ? s.slice(0, 10) + '…' : ''; }
function when(t) { return t ? new Date(t * 1000).toLocaleString() : '미확정'; }
function toast(msg, bad) { const s = $('#status'); s.textContent = msg; s.className = bad ? 'err' : 'muted'; }

async function refreshStatus() {
  try {
    const s = await api('/api/status');
    $('#ver').textContent = `v${s.version} · ${s.network}`;
    $('#status').className = 'muted';
    $('#status').textContent = `높이 ${s.height} · 피어 ${s.peers} · 저장 가격 ${s.storage_price} · 기본 수수료 ${s.base_fee_per_byte}/byte · 대기 ${s.mempool}`;
    const m = s.mining;
    $('#mining').textContent = `파티션 ${m.assigned_partitions} · 패킹 ${m.packed_chunks} · VDF ${m.vdf_steps} · 찾은 블록 ${m.blocks_found} ${m.last_result || ''}`;
    const w = await api('/api/wallet');
    const box = $('#wallet'); box.replaceChildren();
    for (const [k, v] of [['주소', w.address], ['잔액', w.balance], ['잠긴 보상', w.vesting], ['예치', w.stake], ['무료 할당', w.free_quota + ' bytes'], ['보내는 중', w.queued]]) {
      const d = el('div'); d.append(el('span', 'muted', k + ' '), el('span', '', String(v))); box.append(d);
    }
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
  d.append(el('div', 'text', p.text));
  (p.attach || []).forEach((a) => { const img = el('img'); img.src = '/api/data/' + a; img.alt = '첨부'; img.loading = 'lazy';
    img.onerror = () => img.replaceWith(el('div', 'small muted', '첨부를 이 노드가 아직 갖고 있지 않아요 · ' + short(a))); d.append(img); });
  const m = el('div', 'meta');
  m.append(el('span', '', short(p.author)), el('span', '', when(p.time)), el('span', '', p.lane === 'free' ? '무료 차선' : '수수료 차선'));
  if (p.pending) m.append(el('span', '', '전송 대기/미확정'));
  else {
    const like = el('a', '', `좋아요 ${p.likes}`); like.onclick = () => post('/api/like', { id: p.id, lane: 'free' }).then(() => toast('좋아요를 보냈어요')).catch((e) => toast(e.message, true)); m.append(like);
    if (!isComment) { const c = el('a', '', `댓글 ${p.comments}`); c.onclick = () => showThread(p.id); m.append(c); }
    (p.attach || []).forEach((a) => { const x = el('a', '', '첨부 연장'); x.onclick = () => {
      const tier = prompt('연장할 보존 기간 (1y, 5y, 20y, perm)', '5y'); if (tier) post('/api/extend', { root: a, tier }).then(() => toast('연장을 보냈어요')).catch((e) => toast(e.message, true)); }; m.append(x); });
  }
  d.append(m);
  return d;
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
  f.append(ta, b);
  f.onsubmit = (e) => { e.preventDefault(); post('/api/post', { board: t.post.board, text: ta.value, parent: id, lane: 'free' })
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
    if (f.file.files.length) {
      const fd = new FormData(); fd.append('file', f.file.files[0]); fd.append('tier', f.tier.value);
      const r = await api('/api/upload', { method: 'POST', body: fd });
      attach.push(r.root);
    }
    await post('/api/post', { board, text: f.text.value, lane: f.lane.value, attach });
    f.reset(); toast('글을 보냈어요. 잠시 후 미확정 글로 보여요.'); refreshPosts();
  } catch (err) { toast(err.message, true); }
};
$('#stakeForm').onsubmit = (e) => { e.preventDefault(); const op = e.submitter.dataset.op;
  post('/api/' + op, { amount: e.target.amount.value }).then(() => toast(op === 'stake' ? '예치를 보냈어요' : '예치 해제를 보냈어요')).catch((err) => toast(err.message, true)); };
$('#sendForm').onsubmit = (e) => { e.preventDefault();
  post('/api/transfer', { to: e.target.to.value.trim(), amount: e.target.amount.value }).then(() => toast('송금을 보냈어요')).catch((err) => toast(err.message, true)); };

async function tick() { await refreshStatus(); await refreshBoards().catch(() => {}); await refreshPosts().catch(() => {}); if (openThread) showThread(openThread).catch(() => {}); }
tick(); setInterval(tick, 5000);
