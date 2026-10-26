// DISC Podcast & Radio Web Client
'use strict';

const PLACEHOLDER = 'data:image/svg+xml;charset=UTF-8,' + encodeURIComponent(
  '<svg xmlns="http://www.w3.org/2000/svg" width="160" height="160" viewBox="0 0 160 160">' +
  '<rect width="160" height="160" rx="24" fill="#1e293b"/>' +
  '<circle cx="80" cy="80" r="34" fill="none" stroke="#818cf8" stroke-width="6"/>' +
  '<circle cx="80" cy="80" r="8" fill="#818cf8"/>' +
  '<path d="M80 28a52 52 0 0 1 0 104" fill="none" stroke="#6366f1" stroke-width="6"/>' +
  '</svg>'
);

const state = {
  activeTab: 'radio',
  activeCategory: '全部',
  stations: [],
  radioSearchResults: null,
  podcasts: [],
  currentPlaying: null,
  activeFeed: null,
  activeFeedURL: '',
  downloads: {},
  pendingPlay: false,
  lastLogs: [],
  lastLogRefresh: 0,
};

// --- Initialization ---

document.addEventListener('DOMContentLoaded', () => {
  loadStations();
  loadPodcasts();
  pollStatus();

  setInterval(pollStatus, 3000);
  setInterval(pollDownloads, 2500);

  const stationsList = document.getElementById('stations-list');
  if (stationsList) {
    stationsList.addEventListener('click', (event) => {
      const card = event.target.closest('.station-card');
      if (!card) return;
      playRadio(card.dataset.url, card.dataset.name, card.dataset.category, card.dataset.cover, card.dataset.id);
    });
  }

  const categories = document.getElementById('radio-categories');
  if (categories) {
    categories.addEventListener('click', (event) => {
      const chip = event.target.closest('.chip');
      if (!chip) return;
      if (chip.dataset.action === 'add') {
        openAddRadioModal();
      } else if (chip.dataset.action === 'clear-search') {
        clearRadioSearch();
      } else if (chip.dataset.cat !== undefined) {
        filterRadioCategory(chip.dataset.cat);
      }
    });
  }

  const podcastsList = document.getElementById('podcasts-list');
  if (podcastsList) {
    podcastsList.addEventListener('click', (event) => {
      const card = event.target.closest('.podcast-card');
      if (card) openEpisodes(card.dataset.url);
    });
  }

  const episodesView = document.getElementById('episodes-view');
  if (episodesView) {
    episodesView.addEventListener('click', (event) => {
      const button = event.target.closest('[data-action]');
      if (!button) return;
      const action = button.dataset.action;
      if (action === 'play-episode') {
        playPodcast(button.dataset.url, button.dataset.title, button.dataset.feed, button.dataset.cover, button.dataset.id);
      } else if (action === 'download-episode') {
        downloadEpisode(button.dataset.feed, button.dataset.title, button.dataset.url, button.dataset.id);
      } else if (action === 'refresh') {
        openEpisodes(state.activeFeedURL, true);
      }
    });
  }

  const searchInput = document.getElementById('radio-search-input');
  if (searchInput) {
    searchInput.addEventListener('keydown', (event) => {
      if (event.key === 'Enter') searchRadio();
    });
  }

  const podcastInput = document.getElementById('podcast-url-input');
  if (podcastInput) {
    podcastInput.addEventListener('keydown', (event) => {
      if (event.key === 'Enter') subscribePodcast();
    });
  }

  const modal = document.getElementById('add-radio-modal');
  if (modal) {
    modal.addEventListener('click', (event) => {
      if (event.target === modal) closeAddRadioModal();
    });
  }

  bindDiagnosticButtons();
  switchTab('radio');
});

// --- Generic helpers ---

async function api(path, options = {}) {
  const res = await fetch(path, options);
  const text = await res.text();
  let data = null;
  if (text) {
    try {
      data = JSON.parse(text);
    } catch (err) {
      throw new Error(`服务器返回了非 JSON 内容 (HTTP ${res.status})`);
    }
  }
  if (!res.ok) {
    const message = data && (data.error || data.message) ? (data.error || data.message) : `HTTP ${res.status}`;
    throw new Error(message);
  }
  return data;
}

function postJSON(path, body) {
  return api(path, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(body || {}),
  });
}

async function copyText(text) {
  if (navigator.clipboard && window.isSecureContext) {
    await navigator.clipboard.writeText(text);
    return;
  }
  const area = document.createElement('textarea');
  area.value = text;
  area.setAttribute('readonly', '');
  area.style.position = 'fixed';
  area.style.top = '-1000px';
  area.style.opacity = '0';
  document.body.appendChild(area);
  area.select();
  let ok = false;
  try {
    ok = document.execCommand('copy');
  } finally {
    area.remove();
  }
  if (!ok) throw new Error('浏览器不允许自动复制，请手动选择文本');
}

function escapeHtml(value) {
  return String(value === undefined || value === null ? '' : value).replace(/[&<>"']/g, ch => ({
    '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;',
  })[ch]);
}

function escapeAttr(value) {
  return escapeHtml(value);
}

function safeURL(value) {
  if (!value) return '';
  try {
    const parsed = new URL(String(value), window.location.href);
    if (parsed.protocol === 'http:' || parsed.protocol === 'https:' || parsed.protocol === 'data:') {
      return parsed.href;
    }
  } catch (err) {
    // Ignore malformed cover/audio URLs.
  }
  return '';
}

function coverImg(url, className, alt) {
  const src = safeURL(url) || PLACEHOLDER;
  return `<img${className ? ` class="${escapeAttr(className)}"` : ''} src="${escapeAttr(src)}" alt="${escapeAttr(alt || '')}" loading="lazy" onerror="this.onerror=null;this.src='${PLACEHOLDER}'">`;
}

function showToast(message, type = 'info', timeout = 4200) {
  let stack = document.getElementById('toast-stack');
  if (!stack) {
    stack = document.createElement('div');
    stack.id = 'toast-stack';
    stack.className = 'toast-stack';
    document.body.appendChild(stack);
  }
  const toast = document.createElement('div');
  toast.className = `toast toast-${type}`;
  toast.innerHTML = `<span class="toast-mark">${type === 'error' ? '!' : (type === 'success' ? '✓' : 'i')}</span><span>${escapeHtml(message)}</span>`;
  stack.appendChild(toast);
  setTimeout(() => {
    toast.classList.add('toast-hide');
    setTimeout(() => toast.remove(), 260);
  }, timeout);
}

function formatBytes(n) {
  n = Number(n) || 0;
  if (n < 1024) return `${n} B`;
  if (n < 1024 * 1024) return `${(n / 1024).toFixed(1)} KB`;
  if (n < 1024 * 1024 * 1024) return `${(n / 1024 / 1024).toFixed(1)} MB`;
  return `${(n / 1024 / 1024 / 1024).toFixed(2)} GB`;
}

// --- Tabs ---

function switchTab(tab) {
  state.activeTab = tab;
  document.querySelectorAll('.tab-btn').forEach(btn => btn.classList.remove('active'));
  document.querySelectorAll('.tab-content').forEach(content => content.classList.remove('active'));

  const btn = document.getElementById(`tab-${tab}-btn`);
  const content = document.getElementById(`tab-${tab}`);
  if (btn) btn.classList.add('active');
  if (content) content.classList.add('active');

  if (tab === 'podcast') {
    const episodes = document.getElementById('episodes-view');
    const list = document.getElementById('podcasts-list');
    if (state.activeFeed && state.activeFeedURL) {
      if (episodes) episodes.style.display = 'block';
      if (list) list.style.display = 'none';
    } else {
      if (episodes) episodes.style.display = 'none';
      if (list) list.style.display = 'grid';
    }
  }

  if (tab === 'downloads') pollDownloads();
  if (tab === 'diagnostics') {
    loadDebug();
    loadLogs();
  }
}

// --- Radio ---

async function loadStations() {
  try {
    const data = await api('/api/radio/list');
    state.stations = Array.isArray(data) ? data : [];
    state.radioSearchResults = null;
    renderCategories();
    renderStations();
  } catch (err) {
    state.stations = [];
    renderCategories();
    const container = document.getElementById('stations-list');
    if (container) {
      container.innerHTML = `<div class="empty-state"><p>无法读取电台列表</p><span>${escapeHtml(err.message)}</span></div>`;
    }
    showToast('电台列表加载失败: ' + err.message, 'error');
  }
}

function renderCategories() {
  const container = document.getElementById('radio-categories');
  if (!container) return;
  const categories = Array.from(new Set(state.stations.map(s => (s.category || '未分类').trim()).filter(Boolean)));
  const preferred = ['国家台', '新闻资讯', '流行音乐', '财经', '轻音乐/爵士', '古典世界', '国际新闻'];
  categories.sort((a, b) => {
    const ia = preferred.indexOf(a);
    const ib = preferred.indexOf(b);
    if (ia >= 0 || ib >= 0) {
      if (ia < 0) return 1;
      if (ib < 0) return -1;
      return ia - ib;
    }
    return a.localeCompare(b, 'zh-Hans');
  });

  let html = '';
  if (state.radioSearchResults !== null) {
    html += `<button class="chip active" data-action="clear-search">搜索结果 ✕</button>`;
  }
  html += `<button class="chip ${state.radioSearchResults === null && state.activeCategory === '全部' ? 'active' : ''}" data-cat="全部">全部</button>`;
  html += categories.map(cat => {
    const count = state.stations.filter(s => (s.category || '未分类') === cat).length;
    const active = state.radioSearchResults === null && state.activeCategory === cat;
    return `<button class="chip ${active ? 'active' : ''}" data-cat="${escapeAttr(cat)}">${escapeHtml(cat)}${count ? ` ${count}` : ''}</button>`;
  }).join('');
  html += `<button class="chip chip-add" data-action="add">+ 自定义电台</button>`;
  container.innerHTML = html;
}

function filterRadioCategory(cat) {
  state.activeCategory = cat;
  state.radioSearchResults = null;
  const input = document.getElementById('radio-search-input');
  if (input) input.value = '';
  renderCategories();
  renderStations();
}

function clearRadioSearch() {
  state.radioSearchResults = null;
  state.activeCategory = '全部';
  const input = document.getElementById('radio-search-input');
  if (input) input.value = '';
  renderCategories();
  renderStations();
}

function currentStationList() {
  if (state.radioSearchResults !== null) return state.radioSearchResults;
  if (state.activeCategory === '全部') return state.stations;
  return state.stations.filter(s => (s.category || '未分类') === state.activeCategory);
}

function renderStations() {
  const container = document.getElementById('stations-list');
  if (!container) return;
  const list = currentStationList();
  const countBadge = document.getElementById('radio-count');
  const heading = document.getElementById('radio-section-heading');
  if (countBadge) countBadge.textContent = `${list.length} 个`;
  if (heading) heading.textContent = state.radioSearchResults !== null ? '搜索结果' : (state.activeCategory === '全部' ? '全部电台' : `${state.activeCategory}电台`);

  if (!Array.isArray(list) || list.length === 0) {
    container.innerHTML = `
      <div class="empty-state" style="grid-column: 1 / -1;">
        <p>未找到匹配的电台</p>
        <span>尝试搜索其他名称，或使用「+ 自定义电台」添加直播流地址。</span>
      </div>`;
    return;
  }

  const playing = state.currentPlaying || {};
  container.innerHTML = list.map(st => {
    const isPlaying = !!playing.playing && playing.stream_url === st.url;
    const icon = st.favicon ? coverImg(st.favicon, '', st.name) : '📻';
    return `
      <div class="station-card ${isPlaying ? 'active-card' : ''}" data-id="${escapeAttr(st.id || '')}" data-url="${escapeAttr(st.url)}" data-name="${escapeAttr(st.name)}" data-category="${escapeAttr(st.category || '')}" data-cover="${escapeAttr(st.favicon || '')}">
        <div class="station-icon">${icon}</div>
        <div class="station-info">
          <div class="station-name">${escapeHtml(st.name)}</div>
          <div class="station-meta">${escapeHtml(st.category || '网络电台')}${st.bitrate ? ` · ${st.bitrate}k` : ''}${st.codec ? ` · ${escapeHtml(st.codec)}` : ''}</div>
        </div>
        <button class="station-play-btn" title="${isPlaying ? '正在播放' : '播放'}"><span class="play-glyph">${isPlaying ? '❚❚' : '▶'}</span></button>
      </div>`;
  }).join('');
}

async function searchRadio() {
  const input = document.getElementById('radio-search-input');
  const q = input ? input.value.trim() : '';
  if (!q) {
    clearRadioSearch();
    return;
  }
  const countBadge = document.getElementById('radio-count');
  if (countBadge) countBadge.textContent = '搜索中…';
  try {
    const results = await api(`/api/radio/search?q=${encodeURIComponent(q)}`);
    state.radioSearchResults = Array.isArray(results) ? results : [];
    renderCategories();
    renderStations();
    if (state.radioSearchResults.length === 0) showToast('没有找到匹配的电台', 'info');
  } catch (err) {
    state.radioSearchResults = null;
    renderCategories();
    renderStations();
    showToast('搜索失败: ' + err.message, 'error');
  }
}

function openAddRadioModal() {
  const modal = document.getElementById('add-radio-modal');
  if (modal) modal.style.display = 'flex';
}

function closeAddRadioModal() {
  const modal = document.getElementById('add-radio-modal');
  if (modal) modal.style.display = 'none';
}

async function submitCustomRadio() {
  const name = document.getElementById('custom-radio-name').value.trim();
  const url = document.getElementById('custom-radio-url').value.trim();
  const category = document.getElementById('custom-radio-cat').value.trim() || '自定义';
  if (!name || !url) {
    showToast('请填写电台名称和直播流地址', 'error');
    return;
  }
  try {
    const data = await postJSON('/api/radio/add', { name, url, category });
    if (!data || !data.success) throw new Error((data && data.error) || '添加失败');
    closeAddRadioModal();
    document.getElementById('custom-radio-name').value = '';
    document.getElementById('custom-radio-url').value = '';
    await loadStations();
    showToast(`已添加电台：${name}`, 'success');
  } catch (err) {
    showToast('添加失败: ' + err.message, 'error');
  }
}

// --- Podcasts ---

async function loadPodcasts() {
  try {
    const data = await api('/api/podcast/list');
    state.podcasts = Array.isArray(data) ? data : [];
    renderPodcasts();
  } catch (err) {
    state.podcasts = [];
    renderPodcasts();
    showToast('播客订阅加载失败: ' + err.message, 'error');
  }
}

function renderPodcasts() {
  const container = document.getElementById('podcasts-list');
  if (!container) return;
  if (!Array.isArray(state.podcasts) || state.podcasts.length === 0) {
    container.innerHTML = '<div class="empty-state"><p>暂无订阅播客</p><span>在上方粘贴 RSS/Atom 订阅链接即可添加</span></div>';
    return;
  }
  container.innerHTML = state.podcasts.map(pod => {
    const title = pod.title || pod.Title || '未命名播客';
    const cover = pod.cover || pod.Cover || '';
    const author = pod.author || pod.Author || '泛用型播客';
    const desc = pod.description || pod.Description || '';
    const count = Number(pod.episode_count || 0);
    return `
      <div class="podcast-card" data-url="${escapeAttr(pod.url || pod.URL || '')}" data-id="${escapeAttr(pod.id || pod.ID || '')}">
        ${coverImg(cover, 'podcast-cover', title)}
        <div class="podcast-content">
          <div class="podcast-title">${escapeHtml(title)}</div>
          <div class="podcast-author">${escapeHtml(author)}${count ? ` · ${count} 集` : ''}</div>
          <div class="podcast-desc">${escapeHtml(desc)}</div>
        </div>
      </div>`;
  }).join('');
}

async function subscribePodcast() {
  const input = document.getElementById('podcast-url-input');
  const button = document.getElementById('podcast-subscribe-btn');
  const url = input ? input.value.trim() : '';
  if (!url) {
    showToast('请输入播客订阅链接', 'error');
    return;
  }
  if (input) {
    input.disabled = true;
    input.placeholder = '正在下载并解析订阅源，请稍候…';
  }
  if (button) button.disabled = true;
  try {
    const data = await postJSON('/api/podcast/subscribe', { url });
    if (!data || !data.success) throw new Error((data && data.error) || '订阅解析失败');
    if (input) input.value = '';
    await loadPodcasts();
    const feedURL = data.podcast && data.podcast.url ? data.podcast.url : url;
    showToast(`已订阅：${data.podcast ? data.podcast.title : url}`, 'success');
    await openEpisodes(feedURL);
  } catch (err) {
    showToast('订阅失败: ' + err.message, 'error', 7000);
  } finally {
    if (input) {
      input.disabled = false;
      input.placeholder = '输入播客 RSS/Atom 订阅链接（XML）…';
    }
    if (button) button.disabled = false;
  }
}

function setPodcastView(showEpisodes) {
  const list = document.getElementById('podcasts-list');
  const episodes = document.getElementById('episodes-view');
  if (list) list.style.display = showEpisodes ? 'none' : 'grid';
  if (episodes) episodes.style.display = showEpisodes ? 'block' : 'none';
}

async function openEpisodes(feedURL, force = false) {
  if (!feedURL) return;
  state.activeFeedURL = feedURL;
  setPodcastView(true);

  const listContainer = document.getElementById('episodes-list');
  const summaryContainer = document.getElementById('feed-summary');
  if (listContainer) listContainer.innerHTML = '<div class="empty-state"><p>正在加载单集列表…</p><span>首次订阅会下载完整 XML，可能需要几秒</span></div>';
  if (summaryContainer) summaryContainer.innerHTML = `<div class="feed-banner"><div class="feed-banner-info"><h3>正在读取订阅源</h3><p>${escapeHtml(feedURL)}</p></div></div>`;

  try {
    const refresh = force ? '&refresh=1' : '';
    const feed = await api(`/api/podcast/episodes?url=${encodeURIComponent(feedURL)}${refresh}`);
    if (!feed || typeof feed !== 'object') throw new Error('服务器返回的播客数据为空');
    const episodes = Array.isArray(feed.episodes) ? feed.episodes : [];
    state.activeFeed = feed;
    state.activeFeedURL = feed.url || feedURL;

    const title = feed.title || '未命名播客';
    if (summaryContainer) {
      summaryContainer.innerHTML = `
        <div class="feed-banner">
          ${coverImg(feed.cover, 'feed-cover', title)}
          <div class="feed-banner-info">
            <h3>${escapeHtml(title)}</h3>
            <p>${escapeHtml(feed.author || '')}${feed.author ? ' · ' : ''}${episodes.length} 集${feed.last_updated ? ` · ${escapeHtml(feed.last_updated)}` : ''}</p>
            <p class="feed-description">${escapeHtml(feed.description || '')}</p>
            <button class="btn-secondary btn-small" data-action="refresh">刷新单集</button>
          </div>
        </div>`;
    }

    if (episodes.length === 0) {
      if (listContainer) listContainer.innerHTML = '<div class="empty-state"><p>这个订阅暂时没有可播放单集</p><span>请确认订阅链接是播客 RSS/Atom 的 XML 地址</span></div>';
      return;
    }

    const playing = state.currentPlaying || {};
    if (listContainer) {
      listContainer.innerHTML = episodes.map(ep => {
        const audioURL = ep.audio_url || '';
        const isPlaying = !!playing.playing && playing.stream_url === audioURL;
        return `
          <div class="episode-item ${isPlaying ? 'active-card' : ''}" data-url="${escapeAttr(audioURL)}">
            <div class="episode-meta">
              <div class="episode-title">${escapeHtml(ep.title || '未命名单集')}</div>
              <div class="episode-info-row">
                <span>📅 ${escapeHtml(ep.pub_date || '近期')}</span>
                <span>⏱ ${escapeHtml(ep.duration || '音频')}</span>
                ${ep.downloaded ? '<span class="downloaded-label">已下载到 TF 卡</span>' : ''}
              </div>
              ${ep.description ? `<div class="episode-desc">${escapeHtml(ep.description)}</div>` : ''}
            </div>
            <div class="episode-actions">
              <button class="btn-icon" data-action="play-episode" data-url="${escapeAttr(audioURL)}" data-title="${escapeAttr(ep.title || '')}" data-feed="${escapeAttr(title)}" data-cover="${escapeAttr(feed.cover || '')}" data-id="${escapeAttr(ep.id || '')}" title="在 DISC 播放器上播放">
                <span class="play-glyph">${isPlaying ? '❚❚' : '▶'}</span>
              </button>
              <button class="btn-icon ${ep.downloaded ? 'downloaded' : ''}" data-action="download-episode" data-url="${escapeAttr(audioURL)}" data-title="${escapeAttr(ep.title || '')}" data-feed="${escapeAttr(title)}" data-id="${escapeAttr(ep.id || '')}" title="${ep.downloaded ? '已下载到 TF 卡' : '下载到 TF 卡离线收听'}">
                ${ep.downloaded ? '✓' : '⬇'}
              </button>
            </div>
          </div>`;
      }).join('');
    }
    updatePlayingClasses();
  } catch (err) {
    state.activeFeed = null;
    if (summaryContainer) {
      summaryContainer.innerHTML = `<div class="feed-banner error-banner"><div class="feed-banner-info"><h3>加载失败</h3><p>${escapeHtml(err.message)}</p><button class="btn-secondary btn-small" data-action="refresh">重试</button></div></div>`;
    }
    if (listContainer) {
      listContainer.innerHTML = `<div class="empty-state"><p>无法读取单集列表</p><span>${escapeHtml(err.message)}</span></div>`;
    }
    showToast('加载播客失败: ' + err.message, 'error', 7000);
  }
}

function closeEpisodes() {
  state.activeFeed = null;
  state.activeFeedURL = '';
  setPodcastView(false);
  renderPodcasts();
}

async function downloadEpisode(podcastTitle, epTitle, audioURL, epID) {
  try {
    const data = await postJSON('/api/podcast/download', {
      podcast_title: podcastTitle,
      title: epTitle,
      audio_url: audioURL,
      episode_id: epID,
    });
    if (!data || !data.success) throw new Error((data && data.error) || '下载提交失败');
    showToast(`已加入下载：${epTitle}`, 'success');
    switchTab('downloads');
    pollDownloads();
  } catch (err) {
    showToast('下载提交失败: ' + err.message, 'error');
  }
}

// --- Player ---

async function playRadio(url, name, category, cover, id) {
  await sendPlay(url, name, category, cover, 'radio', id);
}

async function playPodcast(url, epTitle, podTitle, cover, id) {
  await sendPlay(url, epTitle, podTitle, cover, 'podcast', id);
}

async function sendPlay(url, title, subtitle, cover, type, id) {
  if (!url) {
    showToast('音频地址为空', 'error');
    return;
  }
  state.pendingPlay = true;
  updateDockUI({
    title,
    subtitle,
    cover,
    playing: false,
    renderer_state: 'CONNECTING',
    error: '',
    stream_url: url,
    type,
  }, true);

  try {
    const data = await postJSON('/api/player/play', { url, title, subtitle, cover, type });
    if (!data || !data.success) throw new Error((data && data.error) || '播放请求失败');
    state.pendingPlay = false;
    if (data.status) {
      state.currentPlaying = data.status;
      updateDockUI(data.status);
      updatePlayingClasses();
    }
    showToast(data.status && data.status.playing ? `正在播放：${title}` : `已发送到播放器：${title}`, 'success');
    setTimeout(pollStatus, 700);
    setTimeout(pollStatus, 2200);
  } catch (err) {
    state.pendingPlay = false;
    updateDockUI({
      title, subtitle, cover, playing: false, renderer_state: 'ERROR',
      error: err.message, stream_url: url, type,
    });
    showToast('播放失败: ' + err.message, 'error', 9000);
    setTimeout(pollStatus, 700);
  }
}

async function togglePlayback() {
  const st = state.currentPlaying || {};
  if (st.playing) {
    try {
      const data = await postJSON('/api/player/stop', {});
      if (data && data.status) {
        state.currentPlaying = data.status;
        updateDockUI(data.status);
      } else {
        updateDockUI({ ...st, playing: false, renderer_state: 'STOPPED' });
      }
      setTimeout(pollStatus, 500);
    } catch (err) {
      showToast('停止失败: ' + err.message, 'error');
    }
  } else if (st.stream_url) {
    await sendPlay(st.stream_url, st.title, st.subtitle, st.cover, st.type || 'radio');
  } else {
    showToast('还没有选择电台或播客', 'info');
  }
}

function updateDockUI(st, pending = false) {
  const titleEl = document.getElementById('dock-title');
  const subEl = document.getElementById('dock-subtitle');
  const coverEl = document.getElementById('dock-cover');
  const btn = document.getElementById('dock-play-btn');
  const dot = document.getElementById('status-dot');
  const statusText = document.getElementById('status-text');
  if (!titleEl || !subEl || !coverEl || !btn || !dot || !statusText) return;

  if (st.title) titleEl.textContent = st.title;
  if (st.subtitle) subEl.textContent = st.subtitle;
  else if (st.stream_url) subEl.textContent = st.stream_url;
  else if (!st.error) subEl.textContent = '点击任意电台或播客开始在播放器收听';

  if (st.cover) {
    coverEl.innerHTML = coverImg(st.cover, '', st.title || '');
  } else if (pending || st.playing) {
    coverEl.innerHTML = '📻';
  } else {
    coverEl.innerHTML = '📻';
  }

  if (pending) {
    btn.innerHTML = '<span class="dock-spinner">…</span>';
    dot.className = 'status-dot connecting';
    statusText.textContent = '连接播放器…';
    subEl.classList.remove('error-text');
    return;
  }

  if (st.playing) {
    btn.innerHTML = '<svg viewBox="0 0 24 24" width="22" height="22" fill="currentColor"><rect x="6" y="4" width="4" height="16"></rect><rect x="14" y="4" width="4" height="16"></rect></svg>';
    dot.className = 'status-dot playing';
    statusText.textContent = '正在播放';
    subEl.classList.remove('error-text');
  } else {
    btn.innerHTML = '<svg viewBox="0 0 24 24" width="22" height="22" fill="currentColor"><polygon points="5 3 19 12 5 21 5 3"></polygon></svg>';
    if (st.error) {
      dot.className = 'status-dot error';
      statusText.textContent = '播放失败';
      subEl.textContent = st.error;
      subEl.classList.add('error-text');
    } else if (st.renderer_found === false) {
      dot.className = 'status-dot error';
      statusText.textContent = '未找到播放器';
      subEl.textContent = st.renderer_error || '请确认 mq_player 已启动';
      subEl.classList.add('error-text');
    } else if (st.renderer_state === 'PAUSED_PLAYBACK') {
      dot.className = 'status-dot connected';
      statusText.textContent = '已暂停';
      subEl.classList.remove('error-text');
    } else {
      dot.className = 'status-dot connected';
      statusText.textContent = '连接就绪';
      subEl.classList.remove('error-text');
    }
  }
}

function updatePlayingClasses() {
  const st = state.currentPlaying || {};
  document.querySelectorAll('.station-card').forEach(card => {
    const active = !!st.playing && card.dataset.url === st.stream_url;
    card.classList.toggle('active-card', active);
    const glyph = card.querySelector('.play-glyph');
    if (glyph) glyph.textContent = active ? '❚❚' : '▶';
  });
  document.querySelectorAll('.episode-item').forEach(item => {
    const active = !!st.playing && item.dataset.url === st.stream_url;
    item.classList.toggle('active-card', active);
    const glyph = item.querySelector('.play-glyph');
    if (glyph) glyph.textContent = active ? '❚❚' : '▶';
  });
}

async function pollStatus() {
  try {
    const st = await api('/api/status');
    state.currentPlaying = st || {};
    updateDockUI(st || {}, state.pendingPlay);
    updatePlayingClasses();
  } catch (err) {
    const dot = document.getElementById('status-dot');
    const statusText = document.getElementById('status-text');
    if (dot) dot.className = 'status-dot error';
    if (statusText) statusText.textContent = '服务未连接';
  }
}

// --- Downloads ---

async function pollDownloads() {
  if (state.activeTab !== 'downloads') return;
  try {
    const data = await api('/api/downloads');
    state.downloads = data && typeof data === 'object' && !Array.isArray(data) ? data : {};
    renderDownloads();
  } catch (err) {
    const container = document.getElementById('downloads-list');
    if (container) container.innerHTML = `<div class="empty-state"><p>下载状态读取失败</p><span>${escapeHtml(err.message)}</span></div>`;
  }
}

function renderDownloads() {
  const container = document.getElementById('downloads-list');
  if (!container) return;
  const items = Object.values(state.downloads || {});
  if (items.length === 0) {
    container.innerHTML = `
      <div class="empty-state">
        <p>暂无正在下载的任务</p>
        <span>在播客单集列表中点击【⬇】，文件会保存到 TF 卡【流媒体下载/播客】目录。</span>
      </div>`;
    return;
  }
  container.innerHTML = items.map(item => {
    const done = item.status === 'completed';
    const failed = item.status === 'failed';
    const progress = Math.max(0, Math.min(100, Number(item.progress) || 0));
    let stateText = `${progress.toFixed(0)}%`;
    if (done) stateText = `✓ 下载完成 · ${formatBytes(item.written)}`;
    else if (failed) stateText = '下载失败';
    return `
      <div class="download-card">
        <div class="download-title-row">
          <span>${escapeHtml(item.title || item.episode_id || '未命名单集')}</span>
          <span class="${done ? 'download-ok' : (failed ? 'download-fail' : 'download-running')}">${escapeHtml(stateText)}</span>
        </div>
        <div class="progress-bar-bg"><div class="progress-bar-fill" style="width: ${done ? 100 : progress}%"></div></div>
        ${item.error ? `<div class="download-error">${escapeHtml(item.error)}</div>` : ''}
        ${item.path ? `<div class="download-path">${escapeHtml(item.path)}</div>` : ''}
      </div>`;
  }).join('');
}

// --- Diagnostics ---

function bindDiagnosticButtons() {
  const actions = {
    'diag-refresh-btn': () => { loadDebug(); loadLogs(); },
    'diag-discover-btn': discoverRenderer,
    'diag-probe-btn': deepProbeRenderer,
    'diag-playlists-btn': rebuildPlaylists,
    'diag-db-btn': exportPlayerDB,
    'diag-copy-btn': copyDebug,
    'renderer-save-btn': saveRendererLocation,
    'renderer-clear-btn': clearRendererLocation,
    'logs-refresh-btn': loadLogs,
    'logs-copy-btn': copyLogs,
    'logs-clear-btn': clearLogs,
  };
  Object.entries(actions).forEach(([id, fn]) => {
    const el = document.getElementById(id);
    if (el) el.addEventListener('click', fn);
  });
  const level = document.getElementById('log-level');
  if (level) level.addEventListener('change', loadLogs);
}

async function loadDebug() {
  const pre = document.getElementById('debug-json');
  const summary = document.getElementById('debug-summary');
  try {
    const data = await api('/api/debug');
    if (pre) pre.textContent = JSON.stringify(data, null, 2);
    const input = document.getElementById('renderer-location');
    if (input && document.activeElement !== input) input.value = data.renderer_override || '';
    if (summary) {
      const renderer = data.renderer ? `${data.renderer.friendly_name || 'renderer'} (${data.renderer.control_url || ''})` : (data.renderer_error || '未发现');
      const st = data.status || {};
      summary.innerHTML = `
        <span class="diag-badge ${data.renderer_found ? 'ok' : 'bad'}">${data.renderer_found ? '播放器已连接' : '未找到播放器'}</span>
        <span class="diag-badge ${st.playing ? 'ok' : ''}">${st.playing ? '正在播放' : '空闲'}</span>
        <span class="diag-badge">播放列表 ${Number(data.playlist_file_count) || 0} 个</span>
        <span class="diag-badge">占位音频 ${Number(data.placeholder_count) || 0} 个</span>
        <span class="diag-badge">活动代理 ${Number(data.active_streams) || 0}</span>
        <div class="diag-line">Renderer: ${escapeHtml(renderer)}</div>
        <div class="diag-line">播放列表目录: ${escapeHtml(data.playlist_root || '')}</div>
        <div class="diag-line">占位音频目录: ${escapeHtml(data.placeholder_root || '')}</div>
        <div class="diag-line">日志文件: ${escapeHtml(data.log_file || '')}</div>
        ${data.renderer_override ? `<div class="diag-line">手动地址: ${escapeHtml(data.renderer_override)}</div>` : ''}
        ${data.last_probe ? `<div class="diag-line">最近探测: ${escapeHtml(data.last_probe)}</div>` : ''}
        ${data.last_stream_error ? `<div class="diag-line">最近流错误: ${escapeHtml(data.last_stream_error)}</div>` : ''}
        ${data.playlist_error ? `<div class="diag-line">播放列表错误: ${escapeHtml(data.playlist_error)}</div>` : ''}
        <div class="diag-line">${st.error ? '最近错误: ' + escapeHtml(st.error) : ''}</div>`;
    }
  } catch (err) {
    if (pre) pre.textContent = '读取失败: ' + err.message;
    if (summary) summary.innerHTML = `<span class="diag-badge bad">读取失败</span><div class="diag-line">${escapeHtml(err.message)}</div>`;
  }
}

async function loadLogs() {
  const container = document.getElementById('logs-list');
  const meta = document.getElementById('logs-meta');
  if (!container) return;
  const level = document.getElementById('log-level') ? document.getElementById('log-level').value : '';
  try {
    const data = await api(`/api/logs?limit=500${level ? `&level=${encodeURIComponent(level)}` : ''}`);
    const entries = data && Array.isArray(data.entries) ? data.entries : [];
    state.lastLogs = entries;
    if (meta) meta.textContent = `${entries.length} 条 · ${data && data.file ? data.file : ''}`;
    if (entries.length === 0) {
      container.innerHTML = '<div class="empty-state"><p>暂无日志</p><span>播放或发现播放器时会产生日志</span></div>';
      return;
    }
    container.innerHTML = entries.map(entry => `
      <div class="log-entry log-${escapeAttr(entry.level || 'info')}">
        <span class="log-time">${escapeHtml(entry.time || '')}</span>
        <span class="log-level">${escapeHtml((entry.level || 'info').toUpperCase())}</span>
        <span class="log-message">${escapeHtml(entry.message || '')}</span>
      </div>`).join('');
  } catch (err) {
    container.innerHTML = `<div class="empty-state"><p>日志读取失败</p><span>${escapeHtml(err.message)}</span></div>`;
  }
}

async function discoverRenderer() {
  const button = document.getElementById('diag-discover-btn');
  if (button) button.disabled = true;
  try {
    const data = await postJSON('/api/renderer/discover', {});
    if (!data || !data.success) throw new Error((data && data.error) || '未发现播放器');
    showToast(`已发现播放器: ${data.renderer && data.renderer.friendly_name ? data.renderer.friendly_name : 'mq_player'}`, 'success');
  } catch (err) {
    showToast('发现播放器失败: ' + err.message, 'error', 8000);
  } finally {
    if (button) button.disabled = false;
    loadDebug();
    loadLogs();
  }
}

async function deepProbeRenderer() {
  const button = document.getElementById('diag-probe-btn');
  const pre = document.getElementById('debug-json');
  if (button) button.disabled = true;
  try {
    const data = await api('/api/diagnostics/renderer');
    const diag = (data && data.diagnostics) || data;
    if (pre) pre.textContent = JSON.stringify(diag, null, 2);
    const found = data && data.renderer_found;
    showToast(found ? '深度检测完成：已找到播放器' : '深度检测完成：未找到播放器，请复制 JSON 反馈', found ? 'success' : 'error', 8000);
  } catch (err) {
    showToast('深度检测失败: ' + err.message, 'error', 8000);
  } finally {
    if (button) button.disabled = false;
    loadDebug();
    loadLogs();
  }
}

async function exportPlayerDB() {
  const button = document.getElementById('diag-db-btn');
  const pre = document.getElementById('debug-json');
  if (button) button.disabled = true;
  try {
    const data = await api('/api/diagnostics/player-db');
    const db = data && data.database ? data.database : data;
    if (pre) pre.textContent = JSON.stringify(data, null, 2);
    if (db && db.copied_to) {
      showToast('播放器数据库已复制到 TF 卡: ' + db.copied_to, 'success', 10000);
    } else {
      showToast('导出播放器数据库失败: ' + ((db && db.error) || '文件不存在'), 'error', 10000);
    }
  } catch (err) {
    showToast('导出播放器数据库失败: ' + err.message, 'error', 10000);
  } finally {
    if (button) button.disabled = false;
    loadDebug();
    loadLogs();
  }
}

async function saveRendererLocation() {
  const input = document.getElementById('renderer-location');
  const button = document.getElementById('renderer-save-btn');
  const location = input ? input.value.trim() : '';
  if (!location) {
    showToast('请先填写播放器地址', 'error');
    return;
  }
  if (button) button.disabled = true;
  try {
    const data = await postJSON('/api/renderer', { location });
    if (!data || !data.success) throw new Error((data && data.error) || '连接失败');
    if (input) input.value = data.renderer_override || location;
    showToast('播放器地址已保存并连接成功', 'success');
  } catch (err) {
    showToast('保存播放器地址失败: ' + err.message, 'error', 10000);
  } finally {
    if (button) button.disabled = false;
    loadDebug();
    loadLogs();
  }
}

async function clearRendererLocation() {
  const button = document.getElementById('renderer-clear-btn');
  if (button) button.disabled = true;
  try {
    await api('/api/renderer', { method: 'DELETE' });
    const input = document.getElementById('renderer-location');
    if (input) input.value = '';
    showToast('已恢复自动发现播放器', 'success');
  } catch (err) {
    showToast('恢复自动发现失败: ' + err.message, 'error');
  } finally {
    if (button) button.disabled = false;
    loadDebug();
    loadLogs();
  }
}

async function rebuildPlaylists() {
  const button = document.getElementById('diag-playlists-btn');
  if (button) button.disabled = true;
  try {
    const data = await postJSON('/api/playlists/build', {});
    if (!data || !data.success) throw new Error((data && data.error) || '重建失败');
    showToast(`播放列表已重建，共 ${data.count} 个文件`, 'success');
  } catch (err) {
    showToast('重建播放列表失败: ' + err.message, 'error', 8000);
  } finally {
    if (button) button.disabled = false;
    loadDebug();
  }
}

async function clearLogs() {
  try {
    await postJSON('/api/logs/clear', {});
    showToast('日志缓冲区已清空', 'success');
    loadLogs();
  } catch (err) {
    showToast('清空日志失败: ' + err.message, 'error');
  }
}

async function copyDebug() {
  const pre = document.getElementById('debug-json');
  if (!pre) return;
  try {
    await copyText(pre.textContent || '');
    showToast('诊断信息已复制', 'success');
  } catch (err) {
    const range = document.createRange();
    range.selectNodeContents(pre);
    const selection = window.getSelection();
    selection.removeAllRanges();
    selection.addRange(range);
    showToast('请按 Ctrl+C 复制选中的诊断信息', 'info');
  }
}

async function copyLogs() {
  const entries = Array.isArray(state.lastLogs) ? state.lastLogs : [];
  if (entries.length === 0) {
    showToast('当前没有日志可复制', 'info');
    return;
  }
  const text = entries.map(e => `${e.time || ''} [${e.level || 'info'}] ${e.message || ''}`).join('\n');
  try {
    await copyText(text);
    showToast(`已复制 ${entries.length} 条日志`, 'success');
  } catch (err) {
    showToast('复制日志失败: ' + err.message, 'error');
  }
}
