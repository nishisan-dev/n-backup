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
    let currentRequestId = 0;
    let currentAbortController = null;
    let configLoaded = false;

    // Ring buffer de dados de throughput por sessão
    // Chave: session_id → { net: number[], disk: number[], lastBytes: number, lastDisk: number }
    const sparkHistory = {};

    // Ring buffer de throughput global (MB/s agregado de todas as sessões)
    const globalThroughput = [];
    let lastGlobalTraffic = null;
    let smoothedGlobalThroughput = null;

    const SMOOTHING_ALPHA = 0.28;

    // Cache dos eventos carregados (para export)
    let cachedEvents = [];

    function cancelInFlightRequest() {
        if (currentAbortController) {
            currentAbortController.abort();
            currentAbortController = null;
        }
    }

    function beginViewRequest() {
        cancelInFlightRequest();
        currentRequestId += 1;
        currentAbortController = new AbortController();
        return {
            requestId: currentRequestId,
            signal: currentAbortController.signal,
        };
    }

    function isLatestRequest(requestId) {
        return requestId === currentRequestId;
    }

    function isAbortError(err) {
        return err && err.name === 'AbortError';
    }

    function pruneSparkHistory(activeSessionIds) {
        const activeIds = new Set(activeSessionIds);
        Object.keys(sparkHistory).forEach((sessionId) => {
            if (!activeIds.has(sessionId)) {
                delete sparkHistory[sessionId];
            }
        });
    }

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
        const { requestId, signal } = beginViewRequest();
        try {
            const [health, metrics, sessions, agents, storages] = await Promise.all([
                API.health({ signal }),
                API.metrics({ signal }),
                API.sessions({ signal }),
                API.agents({ signal }),
                API.storages({ signal }),
            ]);
            if (!isLatestRequest(requestId)) return;

            updateConnectionStatus('connected');
            sessions.forEach(s => updateSparkHistory(s));
            pruneSparkHistory(sessions.map(s => s.session_id));

            Components.renderServerInfo(health);
            Components.renderOverviewMetrics(metrics);
            Components.renderChunkBufferCard(metrics.chunk_buffer || null);
            Components.renderOverviewAgents(agents);
            Components.renderOverviewStorages(storages);
            Components.renderOverviewSessions(sessions);

            // Throughput global: calcula delta de traffic_in total
            if (lastGlobalTraffic !== null) {
                const delta = Math.max(0, metrics.traffic_in_bytes - lastGlobalTraffic);
                const mbps = delta / (1024 * 1024) / (POLL_INTERVAL / 1000);
                smoothedGlobalThroughput = smoothEwma(smoothedGlobalThroughput, mbps);
                globalThroughput.push(smoothedGlobalThroughput);
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
            if (isAbortError(err) || !isLatestRequest(requestId)) return;
            updateConnectionStatus('error');
            console.error('fetchOverview error:', err);
        }
    }

    async function fetchSessions() {
        const { requestId, signal } = beginViewRequest();
        try {
            const [sessions, history] = await Promise.all([
                API.sessions({ signal }),
                API.sessionsHistory({ signal }),
            ]);
            if (!isLatestRequest(requestId)) return;
            updateConnectionStatus('connected');

            // Atualiza ring buffers para cada sessão
            sessions.forEach(s => updateSparkHistory(s));
            pruneSparkHistory(sessions.map(s => s.session_id));

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
            if (isAbortError(err) || !isLatestRequest(requestId)) return;
            updateConnectionStatus('error');
            console.error('fetchSessions error:', err);
        }
    }

    async function fetchSessionDetail(id) {
        const { requestId, signal } = beginViewRequest();
        try {
            const detail = await API.session(id, { signal });
            if (!isLatestRequest(requestId)) return;
            updateConnectionStatus('connected');
            updateSparkHistory(detail);
            Components.renderSessionDetail(detail);

            // Desenha sparklines após render
            drawSessionSparklines(id);
        } catch (err) {
            if (isAbortError(err) || !isLatestRequest(requestId)) return;
            if (err.status === 404 && selectedSessionId === id) {
                selectedSessionId = null;
                document.getElementById('sessions-list').style.display = '';
                document.getElementById('session-detail').style.display = 'none';
                pushRoute('sessions');
                fetchSessions();
                return;
            }
            updateConnectionStatus('error');
            console.error('fetchSessionDetail error:', err);
        }
    }

    async function fetchEvents() {
        const { requestId, signal } = beginViewRequest();
        try {
            const limit = parseInt(document.getElementById('events-limit').value) || 50;
            const events = await API.events(limit, { signal });
            if (!isLatestRequest(requestId)) return;
            updateConnectionStatus('connected');
            cachedEvents = events; // cache para export
            Components.renderEvents(events);
        } catch (err) {
            if (isAbortError(err) || !isLatestRequest(requestId)) return;
            updateConnectionStatus('error');
            console.error('fetchEvents error:', err);
        }
    }

    async function fetchConfig() {
        if (configLoaded) {
            cancelInFlightRequest();
            return;
        }
        const { requestId, signal } = beginViewRequest();
        try {
            const cfg = await API.config({ signal });
            if (!isLatestRequest(requestId)) return;
            updateConnectionStatus('connected');
            configLoaded = true;
            Components.renderConfig(cfg);
        } catch (err) {
            if (isAbortError(err) || !isLatestRequest(requestId)) return;
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
            if (isVisible && currentView !== 'config') {
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
            showSessionDetail(row.dataset.session);
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
        if (isVisible && currentView !== 'config') {
            fetchCurrentView(); // Fetch imediato ao voltar
        }
    });

    // Hash change (browser back/forward + manual edit)
    window.addEventListener('hashchange', handleHashChange);

    // ============ Sparkline Data ============

    function updateSparkHistory(session) {
        const id = session.session_id;
        if (!sparkHistory[id]) {
            sparkHistory[id] = { net: [], disk: [], buf: [], lastBytes: session.bytes_received, lastDisk: session.disk_write_bytes || 0, smoothNet: null, smoothDisk: null };
            return;
        }

        const h = sparkHistory[id];
        const intervalSecs = POLL_INTERVAL / 1000;

        // Calcula delta em MB/s para rede
        const netDelta = Math.max(0, session.bytes_received - h.lastBytes);
        const netMBps = netDelta / (1024 * 1024) / intervalSecs;
        h.smoothNet = smoothEwma(h.smoothNet, netMBps);
        h.net.push(h.smoothNet);
        h.lastBytes = session.bytes_received;

        // Calcula delta em MB/s para disco
        const diskBytes = session.disk_write_bytes || 0;
        const diskDelta = Math.max(0, diskBytes - h.lastDisk);
        const diskMBps = diskDelta / (1024 * 1024) / intervalSecs;
        h.smoothDisk = smoothEwma(h.smoothDisk, diskMBps);
        h.disk.push(h.smoothDisk);
        h.lastDisk = diskBytes;

        // Buffer em voo — nível absoluto em MB (não delta)
        if (session.buffer_enabled) {
            const bufMB = (session.buffer_in_flight_bytes || 0) / (1024 * 1024);
            h.buf.push(bufMB);
            if (h.buf.length > SPARK_MAX_POINTS) h.buf.shift();
        }

        // Trim to max points
        if (h.net.length > SPARK_MAX_POINTS) h.net.shift();
        if (h.disk.length > SPARK_MAX_POINTS) h.disk.shift();
    }

    function smoothEwma(previous, current) {
        if (previous == null) return current;
        return previous + SMOOTHING_ALPHA * (current - previous);
    }

    function drawSessionSparklines(sessionId) {
        const hist = sparkHistory[sessionId];
        if (!hist) return;

        const netCanvas = document.getElementById('spark-net');
        const diskCanvas = document.getElementById('spark-disk');
        const bufCanvas = document.getElementById('spark-buffer');

        if (netCanvas && hist.net.length > 1) Components.drawSparkline(netCanvas, hist.net, '#6366f1');
        if (diskCanvas && hist.disk.length > 1) Components.drawSparkline(diskCanvas, hist.disk, '#10b981');
        if (bufCanvas && hist.buf && hist.buf.length > 1) Components.drawSparkline(bufCanvas, hist.buf, '#06b6d4');
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
