// app.js — Router + Polling da SPA de observabilidade

(function () {
    'use strict';

    const POLL_INTERVAL = 2000;
    const SPARK_MAX_POINTS = 1800; // 1 hora @ 2s poll
    let pollTimer = null;
    let currentView = 'overview';
    let selectedSessionId = null;
    let isVisible = true;
    let isNavigating = false; // guard contra loops hash ↔ switchView

    // Ring buffer de dados de throughput por sessão
    // Chave: session_id → { net: number[], disk: number[], lastBytes: number, lastDisk: number }
    const sparkHistory = {};

    // Ring buffer de throughput global (MB/s agregado de todas as sessões)
    const globalThroughput = [];
    let lastGlobalTraffic = null;

    // Cache dos eventos carregados (para export)
    let cachedEvents = [];

    // ============ Hash Routing ============

    const VALID_VIEWS = ['overview', 'sessions', 'events', 'config'];

    function pushRoute(hash) {
        if (window.location.hash === '#' + hash) return;
        isNavigating = true;
        window.location.hash = hash;
        isNavigating = false;
    }

    function handleHashChange() {
        if (isNavigating) return;

        const raw = window.location.hash.replace(/^#\/?/, '') || 'overview';
        const parts = raw.split('/');
        const view = parts[0];

        if (!VALID_VIEWS.includes(view)) {
            pushRoute('overview');
            return;
        }

        // Se é deep-link para sessão: #sessions/{id}
        if (view === 'sessions' && parts[1]) {
            if (currentView !== 'sessions') {
                switchView('sessions', true);
            }
            showSessionDetail(decodeURIComponent(parts[1]), true);
        } else {
            switchView(view, true);
        }
    }

    // ============ Navigation ============

    function switchView(view, fromHash) {
        currentView = view;
        selectedSessionId = null;

        document.querySelectorAll('.view').forEach(el => el.classList.remove('active'));
        document.querySelectorAll('.nav-tab').forEach(el => el.classList.remove('active'));

        const viewEl = document.getElementById(`view-${view}`);
        if (viewEl) viewEl.classList.add('active');

        const tabEl = document.querySelector(`.nav-tab[data-view="${view}"]`);
        if (tabEl) tabEl.classList.add('active');

        // Reset session detail view
        if (view === 'sessions') {
            document.getElementById('sessions-list').style.display = '';
            document.getElementById('session-detail').style.display = 'none';
        }

        // Sincroniza hash (somente se disparado por UI, não por hash)
        if (!fromHash) {
            pushRoute(view);
        }

        // Fetch imediatamente ao trocar de view
        fetchCurrentView();
    }

    function showSessionDetail(sessionId, fromHash) {
        selectedSessionId = sessionId;
        document.getElementById('sessions-list').style.display = 'none';
        document.getElementById('session-detail').style.display = '';

        if (!fromHash) {
            pushRoute(`sessions/${encodeURIComponent(sessionId)}`);
        }

        fetchSessionDetail(sessionId);
    }

    // ============ Data Fetching ============

    async function fetchOverview() {
        try {
            const [health, metrics, sessions, agents, storages] = await Promise.all([
                API.health(),
                API.metrics(),
                API.sessions(),
                API.agents(),
                API.storages(),
            ]);

            updateConnectionStatus('connected');

            Components.renderServerInfo(health);
            Components.renderOverviewMetrics(metrics);
            Components.renderOverviewAgents(agents);
            Components.renderOverviewStorages(storages);
            Components.renderOverviewSessions(sessions);

            // Throughput global: calcula delta de traffic_in total
            if (lastGlobalTraffic !== null) {
                const delta = Math.max(0, metrics.traffic_in_bytes - lastGlobalTraffic);
                const mbps = delta / (1024 * 1024) / (POLL_INTERVAL / 1000);
                globalThroughput.push(mbps);
                if (globalThroughput.length > SPARK_MAX_POINTS) globalThroughput.shift();

                const canvas = document.getElementById('global-throughput-canvas');
                if (canvas && globalThroughput.length > 1) {
                    Components.drawSparkline(canvas, globalThroughput, '#06b6d4');
                }
            }
            lastGlobalTraffic = metrics.traffic_in_bytes;

            // Atualiza topbar
            document.getElementById('version-badge').textContent = health.version || 'dev';
        } catch (err) {
            updateConnectionStatus('error');
            console.error('fetchOverview error:', err);
        }
    }

    async function fetchSessions() {
        try {
            const [sessions, history] = await Promise.all([
                API.sessions(),
                API.sessionsHistory(),
            ]);
            updateConnectionStatus('connected');

            // Atualiza ring buffers para cada sessão
            sessions.forEach(s => updateSparkHistory(s));

            Components.renderSessionsList(sessions);
            Components.renderSessionHistory(history);

            // Desenha mini sparklines após render
            sessions.forEach(s => {
                const canvas = document.getElementById(`mini-spark-${s.session_id}`);
                const hist = sparkHistory[s.session_id];
                if (canvas && hist && hist.net.length > 1) {
                    Components.drawSparkline(canvas, hist.net, '#6366f1');
                }
            });
        } catch (err) {
            updateConnectionStatus('error');
            console.error('fetchSessions error:', err);
        }
    }

    async function fetchSessionDetail(id) {
        try {
            const detail = await API.session(id);
            updateConnectionStatus('connected');
            updateSparkHistory(detail);
            Components.renderSessionDetail(detail);

            // Desenha sparklines após render
            drawSessionSparklines(id);
        } catch (err) {
            updateConnectionStatus('error');
            console.error('fetchSessionDetail error:', err);
        }
    }

    async function fetchEvents() {
        try {
            const limit = parseInt(document.getElementById('events-limit').value) || 50;
            const events = await API.events(limit);
            updateConnectionStatus('connected');
            cachedEvents = events; // cache para export
            Components.renderEvents(events);
        } catch (err) {
            updateConnectionStatus('error');
            console.error('fetchEvents error:', err);
        }
    }

    async function fetchConfig() {
        try {
            const cfg = await API.config();
            updateConnectionStatus('connected');
            Components.renderConfig(cfg);
        } catch (err) {
            updateConnectionStatus('error');
            console.error('fetchConfig error:', err);
        }
    }

    function fetchCurrentView() {
        switch (currentView) {
            case 'overview': fetchOverview(); break;
            case 'sessions':
                if (selectedSessionId) {
                    fetchSessionDetail(selectedSessionId);
                } else {
                    fetchSessions();
                }
                break;
            case 'events': fetchEvents(); break;
            case 'config': fetchConfig(); break;
        }
    }

    // ============ Connection Status ============

    function updateConnectionStatus(status) {
        const pill = document.getElementById('conn-status');
        const text = pill.querySelector('.status-text');

        pill.className = 'status-pill';
        switch (status) {
            case 'connected':
                pill.classList.add('connected');
                text.textContent = 'Conectado';
                break;
            case 'error':
                pill.classList.add('error');
                text.textContent = 'Erro';
                break;
            default:
                text.textContent = 'Conectando...';
        }
    }

    // ============ Polling ============

    function startPolling() {
        stopPolling();
        fetchCurrentView();
        pollTimer = setInterval(() => {
            if (isVisible) {
                fetchCurrentView();
            }
        }, POLL_INTERVAL);

        document.getElementById('poll-indicator').textContent = `● Polling ${POLL_INTERVAL / 1000}s`;
        document.getElementById('poll-indicator').classList.remove('paused');
    }

    function stopPolling() {
        if (pollTimer) {
            clearInterval(pollTimer);
            pollTimer = null;
        }
        document.getElementById('poll-indicator').textContent = '○ Pausado';
        document.getElementById('poll-indicator').classList.add('paused');
    }

    // ============ Event Listeners ============

    // Tabs
    document.getElementById('nav-tabs').addEventListener('click', (e) => {
        const tab = e.target.closest('.nav-tab');
        if (tab) switchView(tab.dataset.view);
    });

    // Session click (overview table)
    document.getElementById('overview-sessions-body').addEventListener('click', (e) => {
        const row = e.target.closest('tr[data-session]');
        if (row) {
            switchView('sessions');
            setTimeout(() => showSessionDetail(row.dataset.session), 50);
        }
    });

    // Session click (sessions list)
    document.getElementById('sessions-list').addEventListener('click', (e) => {
        const card = e.target.closest('.session-card[data-session]');
        if (card) showSessionDetail(card.dataset.session);
    });

    // Back button
    document.getElementById('btn-back-sessions').addEventListener('click', () => {
        selectedSessionId = null;
        document.getElementById('sessions-list').style.display = '';
        document.getElementById('session-detail').style.display = 'none';
        pushRoute('sessions');
        fetchSessions();
    });

    // Events limit change
    document.getElementById('events-limit').addEventListener('change', fetchEvents);

    // Visibility change — pause polling when tab hidden
    document.addEventListener('visibilitychange', () => {
        isVisible = !document.hidden;
        if (isVisible) {
            fetchCurrentView(); // Fetch imediato ao voltar
        }
    });

    // Hash change (browser back/forward + manual edit)
    window.addEventListener('hashchange', handleHashChange);

    // ============ Sparkline Data ============

    function updateSparkHistory(session) {
        const id = session.session_id;
        if (!sparkHistory[id]) {
            sparkHistory[id] = { net: [], disk: [], lastBytes: session.bytes_received, lastDisk: session.disk_write_bytes || 0 };
            return;
        }

        const h = sparkHistory[id];
        const intervalSecs = POLL_INTERVAL / 1000;

        // Calcula delta em MB/s para rede
        const netDelta = Math.max(0, session.bytes_received - h.lastBytes);
        const netMBps = netDelta / (1024 * 1024) / intervalSecs;
        h.net.push(netMBps);
        h.lastBytes = session.bytes_received;

        // Calcula delta em MB/s para disco
        const diskBytes = session.disk_write_bytes || 0;
        const diskDelta = Math.max(0, diskBytes - h.lastDisk);
        const diskMBps = diskDelta / (1024 * 1024) / intervalSecs;
        h.disk.push(diskMBps);
        h.lastDisk = diskBytes;

        // Trim to max points
        if (h.net.length > SPARK_MAX_POINTS) h.net.shift();
        if (h.disk.length > SPARK_MAX_POINTS) h.disk.shift();
    }

    function drawSessionSparklines(sessionId) {
        const hist = sparkHistory[sessionId];
        if (!hist) return;

        const netCanvas = document.getElementById('spark-net');
        const diskCanvas = document.getElementById('spark-disk');

        if (netCanvas && hist.net.length > 1) {
            Components.drawSparkline(netCanvas, hist.net, '#6366f1');
        }
        if (diskCanvas && hist.disk.length > 1) {
            Components.drawSparkline(diskCanvas, hist.disk, '#10b981');
        }
    }

    // ============ Theme Toggle ============

    function initTheme() {
        const saved = localStorage.getItem('nbackup-theme');
        if (saved) {
            document.documentElement.setAttribute('data-theme', saved);
        }
        updateThemeIcons();
    }

    function toggleTheme() {
        const current = document.documentElement.getAttribute('data-theme');
        const next = current === 'light' ? 'dark' : 'light';
        document.documentElement.setAttribute('data-theme', next);
        localStorage.setItem('nbackup-theme', next);
        updateThemeIcons();
    }

    function updateThemeIcons() {
        const isLight = document.documentElement.getAttribute('data-theme') === 'light';
        const btn = document.getElementById('theme-toggle');
        if (!btn) return;
        btn.querySelector('.icon-moon').style.display = isLight ? 'none' : '';
        btn.querySelector('.icon-sun').style.display = isLight ? '' : 'none';
    }

    document.getElementById('theme-toggle').addEventListener('click', toggleTheme);

    // ============ Export Events ============

    document.getElementById('btn-export-events').addEventListener('click', () => {
        Components.exportEvents(cachedEvents, 'json');
    });

    // ============ Init ============

    initTheme();

    // Restaura rota da hash (ou fallback para overview)
    const initHash = window.location.hash.replace(/^#\/?/, '');
    if (initHash) {
        handleHashChange();
    } else {
        switchView('overview');
    }

    startPolling();
})();
