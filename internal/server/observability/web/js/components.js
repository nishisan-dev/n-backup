// components.js — Render functions para os componentes da SPA

const Components = {
    // Formata bytes para exibição legível
    formatBytes(bytes) {
        if (bytes === 0 || bytes == null) return '0 B';
        const units = ['B', 'KB', 'MB', 'GB', 'TB'];
        const i = Math.floor(Math.log(bytes) / Math.log(1024));
        return (bytes / Math.pow(1024, i)).toFixed(i > 0 ? 1 : 0) + ' ' + units[i];
    },

    // Formata duração tipo "1h23m45s" para algo legível
    formatUptime(str) {
        if (!str) return '—';
        // Go duration string como "1h23m45.678s"
        const match = str.match(/(?:(\d+)h)?(?:(\d+)m)?(?:(\d+(?:\.\d+)?)s)?/);
        if (!match) return str;
        const h = match[1] || 0;
        const m = match[2] || 0;
        const s = Math.round(parseFloat(match[3] || 0));
        const parts = [];
        if (h > 0) parts.push(`${h}h`);
        if (m > 0) parts.push(`${m}m`);
        if (s > 0 || parts.length === 0) parts.push(`${s}s`);
        return parts.join(' ');
    },

    // Badge de status
    statusBadge(status) {
        const cls = `badge badge-${status || 'idle'}`;
        return `<span class="${cls}">${status || '—'}</span>`;
    },

    // Badge de modo
    modeBadge(mode) {
        const cls = `badge badge-${mode || 'single'}`;
        return `<span class="${cls}">${mode || 'single'}</span>`;
    },

    // Badge de level (eventos)
    levelBadge(level) {
        const cls = `badge badge-${level || 'info'}`;
        return `<span class="${cls}">${level || 'info'}</span>`;
    },

    // Formata timestamp ISO para hora local
    formatTime(iso) {
        if (!iso) return '—';
        try {
            const d = new Date(iso);
            return d.toLocaleTimeString('pt-BR', { hour: '2-digit', minute: '2-digit', second: '2-digit' });
        } catch {
            return iso;
        }
    },

    // Formata timestamp ISO para data+hora local
    formatDateTime(iso) {
        if (!iso) return '—';
        try {
            const d = new Date(iso);
            return d.toLocaleString('pt-BR', { day: '2-digit', month: '2-digit', hour: '2-digit', minute: '2-digit', second: '2-digit' });
        } catch {
            return iso;
        }
    },

    // Renderiza cards de overview com métricas
    renderOverviewMetrics(metrics) {
        document.getElementById('m-traffic').textContent = this.formatBytes(metrics.traffic_in_bytes);
        document.getElementById('m-disk').textContent = this.formatBytes(metrics.disk_write_bytes);
        document.getElementById('m-conns').textContent = metrics.active_conns;
        document.getElementById('m-sessions').textContent = metrics.sessions;
    },

    // Renderiza info do server (health)
    renderServerInfo(health) {
        document.getElementById('srv-uptime').textContent = this.formatUptime(health.uptime);
        document.getElementById('srv-go').textContent = health.go || '—';
        document.getElementById('srv-status').innerHTML = this.statusBadge(health.status === 'ok' ? 'running' : 'degraded');
    },

    // Renderiza tabela resumida de sessões no overview
    renderOverviewSessions(sessions) {
        const card = document.getElementById('overview-sessions-card');
        const body = document.getElementById('overview-sessions-body');

        if (!sessions || sessions.length === 0) {
            card.style.display = 'none';
            return;
        }

        card.style.display = '';
        body.innerHTML = sessions.map(s => `
            <tr class="clickable" data-session="${s.session_id}">
                <td>${this.escapeHtml(s.agent)}</td>
                <td>${this.escapeHtml(s.backup || '—')}</td>
                <td>${this.modeBadge(s.mode)}</td>
                <td>${s.active_streams}${s.max_streams ? '/' + s.max_streams : ''}</td>
                <td>${this.formatBytes(s.bytes_received)}</td>
                <td>${this.statusBadge(s.status)}</td>
            </tr>
        `).join('');
    },

    // Renderiza lista de sessões (view Sessões)
    renderSessionsList(sessions) {
        const container = document.getElementById('sessions-list');

        if (!sessions || sessions.length === 0) {
            container.innerHTML = '<p class="empty-state">Nenhuma sessão ativa.</p>';
            return;
        }

        container.innerHTML = sessions.map(s => `
            <div class="session-card" data-session="${s.session_id}">
                <div class="session-card-info">
                    <span class="session-agent">${this.escapeHtml(s.agent)} — ${this.escapeHtml(s.backup || s.storage)}</span>
                    <div class="session-meta">
                        ${this.modeBadge(s.mode)}
                        ${this.statusBadge(s.status)}
                        <span>Início: ${this.formatTime(s.started_at)}</span>
                        <span>Último I/O: ${this.formatTime(s.last_activity)}</span>
                    </div>
                </div>
                <div class="session-card-stats">
                    <span class="session-bytes">${this.formatBytes(s.bytes_received)}</span>
                    <span class="session-streams">${s.active_streams}${s.max_streams ? '/' + s.max_streams : ''} streams</span>
                </div>
            </div>
        `).join('');
    },

    // Renderiza detalhe de uma sessão
    renderSessionDetail(detail) {
        document.getElementById('detail-title').textContent =
            `${detail.agent} — ${detail.backup || detail.storage}`;

        const infoGrid = document.getElementById('detail-info');
        infoGrid.innerHTML = `
            <div class="info-item"><span class="info-label">Session ID</span><span class="info-value">${detail.session_id}</span></div>
            <div class="info-item"><span class="info-label">Modo</span><span class="info-value">${this.modeBadge(detail.mode)}</span></div>
            <div class="info-item"><span class="info-label">Status</span><span class="info-value">${this.statusBadge(detail.status)}</span></div>
            <div class="info-item"><span class="info-label">Recebido</span><span class="info-value">${this.formatBytes(detail.bytes_received)}</span></div>
            <div class="info-item"><span class="info-label">Início</span><span class="info-value">${this.formatDateTime(detail.started_at)}</span></div>
            <div class="info-item"><span class="info-label">Último I/O</span><span class="info-value">${this.formatDateTime(detail.last_activity)}</span></div>
            ${detail.total_objects ? `<div class="info-item"><span class="info-label">Objetos</span><span class="info-value">${detail.objects_sent || 0} / ${detail.total_objects}${detail.walk_complete ? '' : ' (scanning...)'}</span></div>` : ''}
            ${detail.eta ? `<div class="info-item"><span class="info-label">ETA</span><span class="info-value">${detail.eta}</span></div>` : ''}
        `;

        // Streams
        const streamsTitle = document.getElementById('detail-streams-title');
        const streamsWrap = document.getElementById('detail-streams-wrap');
        const streamsBody = document.getElementById('detail-streams-body');

        if (detail.streams && detail.streams.length > 0) {
            streamsTitle.style.display = '';
            streamsWrap.style.display = '';
            streamsBody.innerHTML = detail.streams.map(st => {
                let streamStatus = 'running';
                if (st.idle_secs > 60) streamStatus = 'degraded';
                else if (st.idle_secs > 10) streamStatus = 'idle';
                if (st.slow_since) streamStatus = 'degraded';

                return `
                    <tr>
                        <td>#${st.index}</td>
                        <td>${this.formatBytes(st.offset_bytes)}</td>
                        <td>${st.mbps.toFixed(2)}</td>
                        <td>${st.idle_secs}s</td>
                        <td>${this.statusBadge(streamStatus)}</td>
                    </tr>
                `;
            }).join('');
        } else {
            streamsTitle.style.display = 'none';
            streamsWrap.style.display = 'none';
        }
    },

    // Renderiza eventos
    renderEvents(events) {
        const body = document.getElementById('events-body');
        const empty = document.getElementById('events-empty');

        if (!events || events.length === 0) {
            body.innerHTML = '';
            empty.style.display = '';
            return;
        }

        empty.style.display = 'none';
        // Inverter para mais recente primeiro
        const sorted = [...events].reverse();
        body.innerHTML = sorted.map(e => `
            <tr>
                <td>${this.formatTime(e.timestamp)}</td>
                <td>${this.levelBadge(e.level)}</td>
                <td>${this.escapeHtml(e.type)}</td>
                <td>${this.escapeHtml(e.agent || '—')}</td>
                <td>${this.escapeHtml(e.message)}</td>
            </tr>
        `).join('');
    },

    // Renderiza config
    renderConfig(cfg) {
        document.getElementById('config-json').textContent = JSON.stringify(cfg, null, 2);
    },

    // XSS protection
    escapeHtml(str) {
        if (!str) return '';
        const map = { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#039;' };
        return String(str).replace(/[&<>"']/g, c => map[c]);
    },
};
