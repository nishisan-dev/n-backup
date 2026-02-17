// components.js — Render functions para os componentes da SPA

const Components = {
    // Desenha um sparkline no canvas usando Canvas 2D nativo.
    // dataPoints: array de números, color: cor da linha (hex/hsl)
    drawSparkline(canvas, dataPoints, color = '#6366f1') {
        if (!canvas || !dataPoints || dataPoints.length < 2) return;

        const ctx = canvas.getContext('2d');
        const dpr = window.devicePixelRatio || 1;
        const rect = canvas.getBoundingClientRect();
        canvas.width = rect.width * dpr;
        canvas.height = rect.height * dpr;
        ctx.scale(dpr, dpr);

        const w = rect.width;
        const h = rect.height;
        const pad = 2;
        const plotW = w - pad * 2;
        const plotH = h - pad * 2;

        const max = Math.max(...dataPoints, 0.01);
        const step = plotW / (dataPoints.length - 1);

        // Gradient fill
        const gradient = ctx.createLinearGradient(0, pad, 0, h);
        gradient.addColorStop(0, color + '33'); // 20% opacity
        gradient.addColorStop(1, color + '00'); // transparent

        ctx.clearRect(0, 0, w, h);

        // Build path
        ctx.beginPath();
        ctx.moveTo(pad, h - pad - (dataPoints[0] / max) * plotH);
        for (let i = 1; i < dataPoints.length; i++) {
            const x = pad + i * step;
            const y = h - pad - (dataPoints[i] / max) * plotH;
            ctx.lineTo(x, y);
        }

        // Fill area
        ctx.lineTo(pad + (dataPoints.length - 1) * step, h - pad);
        ctx.lineTo(pad, h - pad);
        ctx.closePath();
        ctx.fillStyle = gradient;
        ctx.fill();

        // Stroke line
        ctx.beginPath();
        ctx.moveTo(pad, h - pad - (dataPoints[0] / max) * plotH);
        for (let i = 1; i < dataPoints.length; i++) {
            ctx.lineTo(pad + i * step, h - pad - (dataPoints[i] / max) * plotH);
        }
        ctx.strokeStyle = color;
        ctx.lineWidth = 1.5;
        ctx.lineJoin = 'round';
        ctx.stroke();
    },
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

    // Badge de compressão
    compressionBadge(compression) {
        const label = compression === 'zst' ? 'zstd' : (compression || 'gzip');
        const cls = compression === 'zst' ? 'badge badge-info' : 'badge badge-neutral';
        return `<span class="${cls}">${label}</span>`;
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
    // Gera HTML para uma barra de stat (gauge)
    renderStatBar(label, percent) {
        let colorClass = 'low';
        if (percent >= 80) colorClass = 'high';
        else if (percent >= 50) colorClass = 'med';

        return `
            <div class="stat-row">
                <span class="stat-label">${label}</span>
                <div class="stat-track">
                    <div class="stat-fill ${colorClass}" style="width: ${Math.min(percent, 100)}%"></div>
                </div>
                <span class="stat-value">${percent.toFixed(0)}%</span>
            </div>
        `;
    },

    renderOverviewAgents(agents) {
        const card = document.getElementById('overview-agents-card');
        const body = document.getElementById('overview-agents-body');
        const headerRow = card.querySelector('thead tr');

        if (!agents || agents.length === 0) {
            card.style.display = 'none';
            return;
        }

        // Ensure header has correct columns (hacky check/update)
        if (headerRow && !headerRow.innerHTML.includes('Stats')) {
            headerRow.innerHTML = `
                <th>Agent</th>
                <th>IP</th>
                <th>Stats (CPU / Mem / Disk)</th>
                <th>Conectado há</th>
                <th>Keepalive</th>
                <th>Versão</th>
                <th>Sessão</th>
            `;
        }

        card.style.display = '';
        body.innerHTML = agents.map(a => {
            const stats = a.stats
                ? `<div class="stat-group" title="Load Avg: ${a.stats.load_average.toFixed(2)}">
                     ${this.renderStatBar('C', a.stats.cpu_percent)}
                     ${this.renderStatBar('M', a.stats.memory_percent)}
                     ${this.renderStatBar('D', a.stats.disk_usage_percent)}
                   </div>`
                : '<span class="text-muted">—</span>';

            return `
            <tr>
                <td><strong>${this.escapeHtml(a.name)}</strong></td>
                <td>${this.escapeHtml(a.remote_addr)}</td>
                <td>${stats}</td>
                <td>${this.escapeHtml(a.connected_for)}</td>
                <td>${a.keepalive_s}s</td>
                <td>${a.client_version ? `<span class="badge badge-neutral text-xs">${this.escapeHtml(a.client_version)}</span>` : '<span class="text-muted">—</span>'}</td>
                <td>${a.has_session ? '<span class="badge badge-running">backup</span>' : '<span class="badge badge-connected">idle</span>'}</td>
            </tr>
        `}).join('');
    },

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
                    <div class="session-card-header">
                        <div class="session-card-title">
                            <span class="session-agent">${this.escapeHtml(s.agent)}</span>
                            ${s.client_version ? `<span class="badge badge-neutral text-xs">${this.escapeHtml(s.client_version)}</span>` : ''}
                            <span class="session-backup">${this.escapeHtml(s.backup || s.storage)}</span>
                        </div>
                    </div>
                    <div class="session-meta">
                        ${this.modeBadge(s.mode)}
                        ${this.compressionBadge(s.compression)}
                        ${this.statusBadge(s.status)}
                        <span>Início: ${this.formatTime(s.started_at)}</span>
                        <span>Último I/O: ${this.formatTime(s.last_activity)}</span>
                        ${s.eta ? `<span>ETA: ${s.eta}</span>` : ''}
                    </div>
                </div>
                <div class="session-card-stats">
                    <span class="session-bytes">${this.formatBytes(s.bytes_received)}</span>
                    <span class="session-streams">${s.active_streams}${s.max_streams ? '/' + s.max_streams : ''} streams</span>
                    <canvas class="mini-sparkline" id="mini-spark-${s.session_id}"></canvas>
                </div>
            </div>
        `).join('');
    },

    // Renderiza detalhe de uma sessão
    renderSessionDetail(detail) {
        document.getElementById('detail-title').textContent =
            `${detail.agent} — ${detail.backup || detail.storage}`;

        // Progress bar para objetos (se disponível)
        let progressHtml = '';
        if (detail.total_objects && detail.total_objects > 0) {
            const pct = Math.min(100, Math.round((detail.objects_sent / detail.total_objects) * 100));
            progressHtml = `
                <div class="progress-row" style="grid-column: 1 / -1;">
                    <div class="progress-bar-wrap">
                        <div class="progress-bar-fill" style="width: ${pct}%"></div>
                    </div>
                    <span class="progress-text">
                        ${detail.objects_sent} / ${detail.total_objects} objetos (${pct}%)
                        ${detail.walk_complete ? '' : ' — scanning…'}
                        ${detail.eta ? ' — ETA: ' + detail.eta : ''}
                    </span>
                </div>`;
        }

        const infoGrid = document.getElementById('detail-info');
        infoGrid.innerHTML = `
            <div class="info-item"><span class="info-label">Session ID</span><span class="info-value">${detail.session_id}</span></div>
            <div class="info-item"><span class="info-label">Modo</span><span class="info-value">${this.modeBadge(detail.mode)}</span></div>
            <div class="info-item"><span class="info-label">Compressão</span><span class="info-value">${this.compressionBadge(detail.compression)}</span></div>
            <div class="info-item"><span class="info-label">Status</span><span class="info-value">${this.statusBadge(detail.status)}</span></div>
            <div class="info-item"><span class="info-label">Recebido</span><span class="info-value">${this.formatBytes(detail.bytes_received)}</span></div>
            <div class="info-item"><span class="info-label">Disk Write</span><span class="info-value">${this.formatBytes(detail.disk_write_bytes)}</span></div>
            <div class="info-item"><span class="info-label">Início</span><span class="info-value">${this.formatDateTime(detail.started_at)}</span></div>
            <div class="info-item"><span class="info-label">Último I/O</span><span class="info-value">${this.formatDateTime(detail.last_activity)}</span></div>
            ${detail.eta ? `<div class="info-item"><span class="info-label">ETA</span><span class="info-value">${detail.eta}</span></div>` : ''}
            ${detail.client_version ? `<div class="info-item"><span class="info-label">Client Version</span><span class="info-value">${this.escapeHtml(detail.client_version)}</span></div>` : ''}
            ${detail.assembler ? `
                <div class="info-item" style="grid-column: 1/-1; border-top: 1px solid var(--border-color); margin-top: 0.5rem; padding-top: 0.5rem;">
                    <span class="info-label">Assembler Status</span>
                    <span class="info-value text-xs font-mono">
                        Seq: ${detail.assembler.next_expected_seq} |
                        Pending: ${detail.assembler.pending_chunks} chunks (${this.formatBytes(detail.assembler.pending_mem_bytes)}) |
                        Total: ${this.formatBytes(detail.assembler.total_bytes)} |
                        ${detail.assembler.finalized ? '<span class="badge badge-success">Finalized</span>' : '<span class="badge badge-info">Active</span>'}
                    </span>
                </div>
            ` : ''}
            ${progressHtml}
        `;

        // Sparklines section (rede + disk I/O)
        let sparkSection = document.getElementById('detail-sparklines');
        if (!sparkSection) {
            sparkSection = document.createElement('div');
            sparkSection.id = 'detail-sparklines';
            sparkSection.className = 'sparkline-section';
            const detailCard = document.getElementById('session-detail');
            const streamsTitle = document.getElementById('detail-streams-title');
            detailCard.insertBefore(sparkSection, streamsTitle);
        }

        // Calcula throughput agregado
        const totalMbps = (detail.streams || []).reduce((sum, st) => sum + (st.mbps || 0), 0);

        sparkSection.innerHTML = `
            <div class="sparkline-card">
                <div class="sparkline-header">
                    <span class="sparkline-label">Network In</span>
                    <span class="sparkline-value" id="spark-net-val">${totalMbps.toFixed(2)} MB/s</span>
                </div>
                <canvas class="sparkline-canvas" id="spark-net"></canvas>
            </div>
            <div class="sparkline-card">
                <div class="sparkline-header">
                    <span class="sparkline-label">Disk Write</span>
                    <span class="sparkline-value" id="spark-disk-val">${this.formatBytes(detail.disk_write_bytes)}</span>
                </div>
                <canvas class="sparkline-canvas" id="spark-disk"></canvas>
            </div>
        `;

        // Streams
        const streamsTitle = document.getElementById('detail-streams-title');
        const streamsWrap = document.getElementById('detail-streams-wrap');
        const streamsBody = document.getElementById('detail-streams-body');

        if (detail.streams && detail.streams.length > 0) {
            streamsTitle.style.display = '';
            streamsWrap.style.display = '';
            streamsBody.innerHTML = detail.streams.map(st => {
                const streamStatus = st.status || (st.active ? 'running' : 'inactive');

                return `
                    <tr>
                        <td>#${st.index}</td>
                        <td>${this.formatBytes(st.offset_bytes)}</td>
                        <td>${st.mbps.toFixed(2)}</td>
                        <td>${st.idle_secs}s</td>
                        <td>${this.formatUptime(st.connected_for)}</td>
                        <td>${st.reconnects > 0 ? '<span class="badge badge-warn">' + st.reconnects + '</span>' : '0'}</td>
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
