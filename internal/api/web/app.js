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
    for (const [k, v] of [['주소', w.address], ['잔액', w.balance], ['잠긴 보상', w.vesting], ['예치', w.stake], ['숙성 중 예치', w.stake_matures_at ? `${w.stake_pending} (높이 ${w.stake_matures_at}에 숙성)` : w.stake_pending], ['보내는 중', w.queued]]) {
      const d = el('div'); d.append(el('span', 'muted', k + ' '), el('span', '', String(v))); box.append(d);
    }
    const r = await api('/api/rln');
    const names = { none: '등록 안 됨', pending: '등록 대기', active: '활성', leaving: '해제 중', gone: '종료됨' };
    const rb = $('#rln'); rb.replaceChildren();
    const rows = [['상태', names[r.status] || r.status], ['회원 수', r.members]];
    if (r.member) rows.push(['보증금', r.bond], ['이번 시간', `${r.used} / ${Math.min(r.limit, r.cap)}개 (${r.epoch_blocks_left}블록 뒤 초기화)`]);
    if (r.status === 'leaving') rows.push(['보증금 반환 높이', r.release]);
    if (r.pseudonym) rows.push(['가명', short(r.pseudonym)]);
    for (const [k, v] of rows) { const d = el('div'); d.append(el('span', 'muted', k + ' '), el('span', '', String(v))); rb.append(d); }
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
  d.append(el('div', 'text', p.text));
  (p.attach || []).forEach((a) => { const img = el('img'); img.src = '/api/data/' + a; img.alt = '첨부'; img.loading = 'lazy';
    img.onerror = () => img.replaceWith(el('div', 'small muted', '첨부를 이 노드가 아직 갖고 있지 않아요 · ' + short(a))); d.append(img); });
  const m = el('div', 'meta');
  const who = p.lane === 'anon' ? (p.pseudonym ? '가명 ' + short(p.author) : '익명') : short(p.author);
  m.append(el('span', '', who), el('span', '', when(p.time)), el('span', '', p.lane === 'anon' ? '익명 차선' : '수수료 차선'));
  if (p.pending) m.append(el('span', '', '전송 대기/미확정'));
  else {
    const like = el('a', '', `좋아요 ${p.likes}`); like.onclick = () => post('/api/like', { id: p.id }).then(() => toast('좋아요를 보냈어요')).catch((e) => toast(e.message, true)); m.append(like);
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
  const ln = el('select'); ln.append(new Option('익명', 'anon'), new Option('수수료', 'fee'));
  f.append(ta, ln, b);
  f.onsubmit = (e) => { e.preventDefault(); post('/api/post', { board: t.post.board, text: ta.value, parent: id, lane: ln.value })
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
      const fd = new FormData(); fd.append('file', f.file.files[0]); fd.append('tier', f.tier.value);
      const r = await api('/api/upload', { method: 'POST', body: fd });
      attach.push(r.root);
      if (r.action === 'reuse') note = ' 같은 파일이 이미 저장돼 있어 다시 올리지 않았어요.';
      if (r.action === 'extend') note = ' 같은 파일이 이미 저장돼 있어 보존 기간만 늘렸어요.';
    }
    await post('/api/post', { board, text: f.text.value, lane: f.lane.value, pseudonym: f.pseudonym.checked, attach });
    f.reset(); toast('글을 보냈어요. 잠시 후 미확정 글로 보여요.' + note); refreshPosts();
  } catch (err) { toast(err.message, true); }
};
$('#stakeForm').onsubmit = (e) => { e.preventDefault(); const op = e.submitter.dataset.op;
  post('/api/' + op, { amount: e.target.amount.value }).then(() => toast(op === 'stake' ? '예치를 보냈어요' : '예치 해제를 보냈어요')).catch((err) => toast(err.message, true)); };
$('#rlnJoinForm').onsubmit = (e) => { e.preventDefault();
  post('/api/rln/join', { amount: e.target.amount.value }).then(() => toast('익명 글쓰기 등록을 보냈어요')).catch((err) => toast(err.message, true)); };
$('#rlnLeave').onclick = () => { if (confirm('등록을 해제할까요? 보증금은 해제 기간이 지난 뒤 돌아와요.'))
  post('/api/rln/leave', {}).then(() => toast('등록 해제를 보냈어요')).catch((err) => toast(err.message, true)); };
$('#sendForm').onsubmit = (e) => { e.preventDefault();
  post('/api/transfer', { to: e.target.to.value.trim(), amount: e.target.amount.value }).then(() => toast('송금을 보냈어요')).catch((err) => toast(err.message, true)); };

async function tick() { await refreshStatus(); await refreshBoards().catch(() => {}); await refreshPosts().catch(() => {}); if (openThread) showThread(openThread).catch(() => {}); }
tick(); setInterval(tick, 5000);
