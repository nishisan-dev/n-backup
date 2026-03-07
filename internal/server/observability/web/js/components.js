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

    // Badge de fase do ciclo de vida da sessão
    phaseBadge(phase) {
        if (!phase) return '';
        const phaseConfig = {
            receiving:  { cls: 'badge-info',       icon: '📥', label: 'receiving' },
            assembling: { cls: 'badge-assembling', icon: '⚙️',  label: 'assembling' },
            verifying:  { cls: 'badge-warn',       icon: '🔍', label: 'verifying' },
            uploading:  { cls: 'badge-uploading',  icon: '☁️',  label: 'uploading' },
            done:       { cls: 'badge-success',    icon: '✓',  label: 'done' },
            failed:     { cls: 'badge-error',      icon: '✗',  label: 'failed' },
        };
        const cfg = phaseConfig[phase] || { cls: 'badge-neutral', icon: '', label: phase };
        return `<span class="badge ${cfg.cls}">${cfg.icon} ${cfg.label}</span>`;
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

        // Server Stats (Fase 5)
        if (health.stats) {
            const s = health.stats;
            document.getElementById('srv-goroutines').textContent = s.goroutines;
            document.getElementById('srv-heap').textContent = s.heap_alloc_mb.toFixed(1) + ' MB';
            document.getElementById('srv-gc').textContent = s.gc_pause_ms.toFixed(2) + ' ms';
            document.getElementById('srv-cpu').textContent = s.cpu_cores;
        }
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

    // Renderiza cards de storage com uso de disco no overview
    renderOverviewStorages(storages) {
        const card = document.getElementById('overview-storages-card');
        const body = document.getElementById('overview-storages-body');

        if (!storages || storages.length === 0) {
            card.style.display = 'none';
            return;
        }

        card.style.display = '';
        body.innerHTML = storages.map(s => {
            const pct = s.usage_percent || 0;
            let colorClass = 'low';
            let criticalClass = '';
            let alertIcon = '';
            if (pct >= 90) {
                colorClass = 'high';
                criticalClass = ' gauge-critical';
                alertIcon = '<span class="alert-icon" title="Disco crítico!">⚠️</span>';
            } else if (pct >= 70) colorClass = 'med';

            return `
            <div class="storage-card${criticalClass}">
                <div class="storage-header">
                    <span class="storage-name">${this.escapeHtml(s.name)}${alertIcon}</span>
                    <div class="storage-badges">
                        ${this.compressionBadge(s.compression_mode === 'zst' ? 'zst' : 'gzip')}
                        <span class="badge badge-neutral">${this.escapeHtml(s.assembler_mode)}</span>
                    </div>
                </div>
                <div class="storage-usage">
                    <div class="stat-track storage-bar">
                        <div class="stat-fill ${colorClass}" style="width: ${Math.min(pct, 100)}%"></div>
                    </div>
                    <span class="storage-pct">${pct.toFixed(1)}%</span>
                </div>
                <div class="storage-details">
                    <span title="Espaço usado">${this.formatBytes(s.used_bytes)} / ${this.formatBytes(s.total_bytes)}</span>
                    <span title="Espaço livre">Livre: ${this.formatBytes(s.free_bytes)}</span>
                </div>
                <div class="storage-meta">
                    <span title="Diretório base">${this.escapeHtml(s.base_dir)}</span>
                    <span>Max: ${s.max_backups} | Backups: ${s.backups_count}</span>
                </div>
            </div>`;
        }).join('');
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
                <td>${this.statusBadge(s.status)} ${this.phaseBadge(s.phase)}</td>
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
                        ${this.phaseBadge(s.phase)}
                        ${s.assembler && s.assembler.phase === 'assembling' ? '<span class="badge badge-assembling">⚙ assembling</span>' : ''}
                        <span>Início: ${this.formatTime(s.started_at)}</span>
                        <span>Último I/O: ${this.formatTime(s.last_activity)}</span>
                        ${s.eta ? `<span>ETA: ${s.eta}</span>` : ''}
                        ${s.assembly_eta ? `<span>Assembly ETA: ${s.assembly_eta}</span>` : ''}
                    </div>
                    ${s.buffer_enabled ? this.renderBufferSessionInline(s) : ''}
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
            ${detail.bytes_received > 0 && detail.disk_write_bytes > 0 ? `<div class="info-item"><span class="info-label">Compression Ratio</span><span class="info-value">${(detail.disk_write_bytes / detail.bytes_received * 100).toFixed(1)}%</span></div>` : ''}
            ${detail.started_at ? `<div class="info-item"><span class="info-label">Duração</span><span class="info-value">${this.formatElapsed(detail.started_at)}</span></div>` : ''}
            ${detail.assembler ? this.renderAssemblerProgress(detail.assembler, detail.assembly_eta) : ''}
            ${detail.auto_scale ? this.renderAutoScaleInfo(detail.auto_scale, detail) : ''}
            ${detail.buffer_enabled ? this.renderBufferMemoryBlock(detail) : ''}
            ${progressHtml}
            ${this.renderLifecycleProgress(detail)}
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

            // Agrupa: ativos primeiro, disabled no meio, disconnected abaixo
            const active = detail.streams.filter(st => st.status !== 'disconnected' && st.status !== 'disabled');
            const disabled = detail.streams.filter(st => st.status === 'disabled');
            const disconnected = detail.streams.filter(st => st.status === 'disconnected');

            const renderRow = (st, dimmedClass) => {
                const streamStatus = st.status || (st.active ? 'running' : 'disconnected');
                const rowClass = dimmedClass ? ` class="${dimmedClass}"` : '';
                const lostBadge = st.chunks_lost > 0
                    ? `<span class="badge badge-error">${st.chunks_lost}</span>`
                    : '0';
                const retxBadge = st.chunks_retransmitted > 0
                    ? `<span class="badge badge-warn">${st.chunks_retransmitted}</span>`
                    : '0';
                const rotBadge = st.rotations > 0
                    ? `<span class="badge badge-info">${st.rotations}</span>`
                    : '0';

                return `
                    <tr${rowClass}>
                        <td>#${st.index}</td>
                        <td>${this.formatBytes(st.offset_bytes)}</td>
                        <td>${st.mbps.toFixed(2)}</td>
                        <td>${st.idle_secs}s</td>
                        <td>${this.formatUptime(st.connected_for)}</td>
                        <td>${st.reconnects > 0 ? '<span class="badge badge-warn">' + st.reconnects + '</span>' : '0'}</td>
                        <td>${rotBadge}</td>
                        <td>${st.chunks_received || 0}</td>
                        <td>${lostBadge}</td>
                        <td>${retxBadge}</td>
                        <td>${st.last_chunk_seq || 0}</td>
                        <td>${this.statusBadge(streamStatus)}</td>
                    </tr>
                `;
            };

            const cols = 12;
            let rows = active.map(st => renderRow(st, '')).join('');
            if (disabled.length > 0) {
                rows += `<tr class="stream-separator"><td colspan="${cols}">${disabled.length} slot${disabled.length > 1 ? 's' : ''} disabled (parked)</td></tr>`;
                rows += disabled.map(st => renderRow(st, 'stream-disabled')).join('');
            }
            if (disconnected.length > 0) {
                rows += `<tr class="stream-separator"><td colspan="${cols}">${disconnected.length} stream${disconnected.length > 1 ? 's' : ''} disconnected</td></tr>`;
                rows += disconnected.map(st => renderRow(st, 'stream-disconnected')).join('');
            }
            streamsBody.innerHTML = rows;
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

    // Formata duração desde um timestamp ISO até agora
    formatElapsed(isoStart) {
        if (!isoStart) return '—';
        try {
            const start = new Date(isoStart);
            const now = new Date();
            const diffMs = now - start;
            const secs = Math.floor(diffMs / 1000);
            const h = Math.floor(secs / 3600);
            const m = Math.floor((secs % 3600) / 60);
            const s = secs % 60;
            const parts = [];
            if (h > 0) parts.push(`${h}h`);
            if (m > 0) parts.push(`${m}m`);
            parts.push(`${s}s`);
            return parts.join(' ');
        } catch {
            return '—';
        }
    },

    // Renderiza seção de auto-scaler stats no detalhe da sessão
    renderAutoScaleInfo(as, detail = null) {
        if (!as) return '';

        // Se já está no máximo de streams, scaling_up não faz sentido — mostra stable
        let effectiveState = as.state;
        if (effectiveState === 'scaling_up' && as.active_streams >= as.max_streams) {
            effectiveState = 'stable';
        }

        // Badge de estado
        const stateMap = {
            stable: { badge: 'badge-running', label: '● stable' },
            scaling_up: { badge: 'badge-info', label: '▲ scaling up' },
            scaling_down: { badge: 'badge-warn', label: '▼ scaling down' },
            probing: { badge: 'badge-probing', label: '◉ probing' },
        };
        const st = stateMap[effectiveState] || stateMap.stable;

        // Efficiency gauge (0-2 range mapeado para 0-100%)
        const effPct = Math.min(100, Math.round(as.efficiency * 50));
        let effColor = 'high'; // < 0.6 = vermelho (crítico)
        if (as.efficiency >= 1.0) effColor = 'low';       // ≥ 1.0 = verde (saudável)
        else if (as.efficiency >= 0.6) effColor = 'med';   // 0.6-1.0 = âmbar (degradando)

        const producerMBs = Number(as.producer_mbs || 0);
        const drainMBs = Number(as.drain_mbs || 0);
        const bufferFill = Number(detail?.buffer_fill_percent || 0);
        const drainBaseline = drainMBs > 0 ? drainMBs : 1;
        const skewPct = ((producerMBs - drainMBs) / drainBaseline) * 100;

        let flowHint = 'Balanced';
        let flowHintClass = 'badge-running';
        let flowHintReason = 'Producer e drain estao proximos, sem sinal forte de backpressure.';
        if (producerMBs < drainMBs * 0.92 && bufferFill < 10) {
            flowHint = 'Producer-Limited';
            flowHintClass = 'badge-warn';
            flowHintReason = 'O agent esta produzindo menos do que os senders conseguem drenar.';
        } else if (drainMBs < producerMBs * 0.92 || bufferFill >= 10) {
            flowHint = 'Transport/Drain-Limited';
            flowHintClass = 'badge-info';
            flowHintReason = 'A drenagem sustentada esta abaixo da producao atual.';
        }

        let streamHint = '';
        if (detail && Array.isArray(detail.streams)) {
            const activeStreams = detail.streams.filter((stream) => stream.active);
            const slowStreams = activeStreams.filter((stream) => stream.status === 'slow');
            if (slowStreams.length > 0 && slowStreams.every((stream) => (stream.idle_secs || 0) < 10)) {
                streamHint = `${slowStreams.length}/${activeStreams.length} streams estao "slow", mas com I/O recente. Isso sugere threshold de vazao, nao idle real.`;
            }
        }

        const probeIndicator = as.probe_active
            ? '<span class="badge badge-probing" style="margin-left: 0.5rem;">probe active</span>'
            : '';

        return `
            <div class="info-item" style="grid-column: 1/-1; border-top: 1px solid var(--border-color); margin-top: 0.5rem; padding-top: 0.5rem;">
                <span class="info-label">Auto-Scaler</span>
                <div class="info-value" style="display: flex; flex-direction: column; gap: 0.5rem;">
                    <div style="display: flex; align-items: center; gap: 0.5rem; flex-wrap: wrap;">
                        <span class="badge ${st.badge}">${st.label}</span>
                        ${probeIndicator}
                        <span class="badge ${flowHintClass}">${flowHint}</span>
                        <span class="text-xs font-mono">
                            ${as.active_streams}/${as.max_streams} streams
                        </span>
                    </div>
                    <div class="autoscale-metrics">
                        <div class="autoscale-metric">
                            <span class="autoscale-metric-label">Efficiency</span>
                            <div class="stat-track">
                                <div class="stat-fill ${effColor}" style="width: ${effPct}%"></div>
                            </div>
                            <span class="autoscale-metric-value">${as.efficiency.toFixed(2)}</span>
                        </div>
                        <div class="autoscale-metric">
                            <span class="autoscale-metric-label">Producer</span>
                            <span class="autoscale-metric-value">${as.producer_mbs.toFixed(2)} MB/s</span>
                        </div>
                        <div class="autoscale-metric">
                            <span class="autoscale-metric-label">Drain</span>
                            <span class="autoscale-metric-value">${as.drain_mbs.toFixed(2)} MB/s</span>
                        </div>
                        <div class="autoscale-metric">
                            <span class="autoscale-metric-label">Skew</span>
                            <span class="autoscale-metric-value">${skewPct >= 0 ? '+' : ''}${skewPct.toFixed(1)}%</span>
                        </div>
                    </div>
                    <div class="text-xs" style="color: var(--text-muted); line-height: 1.45;">
                        ${this.escapeHtml(flowHintReason)}
                        ${streamHint ? `<br>${this.escapeHtml(streamHint)}` : ''}
                    </div>
                </div>
            </div>`;
    },

    // Renderiza barra de progresso do assembler com fase e ETA
    renderAssemblerProgress(asm, assemblyEta) {
        if (!asm) return '';

        const phase = asm.phase || 'receiving';
        const total = asm.total_chunks || 0;
        const assembled = asm.assembled_chunks || 0;

        // Cores por fase
        const phaseConfig = {
            receiving: { color: 'var(--color-indigo)', label: '📥 Receiving', badge: 'badge-info' },
            assembling: { color: 'var(--color-emerald, #10b981)', label: '⚙ Assembling', badge: 'badge-assembling' },
            done: { color: 'var(--color-success, #22c55e)', label: '✓ Finalizado', badge: 'badge-success' },
        };
        const cfg = phaseConfig[phase] || phaseConfig.receiving;

        // Barra de progresso (relevante apenas para assembling e done)
        let progressBar = '';
        if (phase === 'assembling' || phase === 'done') {
            const pct = total > 0 ? Math.min(100, Math.round((assembled / total) * 100)) : 0;
            progressBar = `
                <div class="progress-bar-wrap" style="margin-top: 0.25rem;">
                    <div class="progress-bar-fill" style="width: ${pct}%; background: ${cfg.color};"></div>
                </div>
                <span class="progress-text" style="font-size: 0.75rem;">
                    ${assembled} / ${total} chunks (${pct}%)
                    ${assemblyEta ? ' — ETA: ' + assemblyEta : ''}
                </span>`;
        }

        // Info compacta
        const pendingInfo = asm.pending_chunks > 0
            ? `Pending: ${asm.pending_chunks} (${this.formatBytes(asm.pending_mem_bytes)})`
            : '';

        return `
            <div class="info-item" style="grid-column: 1/-1; border-top: 1px solid var(--border-color); margin-top: 0.5rem; padding-top: 0.5rem;">
                <span class="info-label">Assembly</span>
                <div class="info-value" style="display: flex; flex-direction: column; gap: 0.25rem;">
                    <div style="display: flex; align-items: center; gap: 0.5rem; flex-wrap: wrap;">
                        <span class="${cfg.badge}">${cfg.label}</span>
                        <span class="text-xs font-mono">
                            Seq: ${asm.next_expected_seq} |
                            Total: ${this.formatBytes(asm.total_bytes)}
                            ${pendingInfo ? ' | ' + pendingInfo : ''}
                        </span>
                    </div>
                    ${progressBar}
                </div>
            </div>`;
    },

    // Badge de resultado para sessões finalizadas
    // Renderiza progress bars de lifecycle (Verifying / Uploading) no session detail
    renderLifecycleProgress(detail) {
        if (!detail.phase) return '';
        let html = '';

        // Verifying progress
        if (detail.phase === 'verifying' && detail.integrity_progress) {
            const ip = detail.integrity_progress;
            const pct = ip.progress_pct || 0;
            html += `
                <div class="info-item lifecycle-progress" style="grid-column: 1/-1; border-top: 1px solid var(--border-color); margin-top: 0.5rem; padding-top: 0.5rem;">
                    <span class="info-label">🔍 Integrity Check</span>
                    <div class="info-value" style="display: flex; flex-direction: column; gap: 0.25rem;">
                        <div style="display: flex; align-items: center; gap: 0.5rem; flex-wrap: wrap;">
                            <span class="badge badge-warn">verifying</span>
                            <span class="text-xs font-mono">
                                ${this.formatBytes(ip.bytes_read)} / ${this.formatBytes(ip.total_bytes)}
                                | ${ip.entries} entries
                                ${ip.eta ? ' | ETA: ' + ip.eta : ''}
                            </span>
                        </div>
                        <div class="progress-bar-wrap" style="margin-top: 0.25rem;">
                            <div class="progress-bar-fill" style="width: ${Math.min(pct, 100)}%; background: var(--color-amber, #f59e0b);"></div>
                        </div>
                        <span class="progress-text" style="font-size: 0.75rem;">${pct.toFixed(1)}%</span>
                    </div>
                </div>`;
        }

        // Uploading progress
        if (detail.phase === 'uploading' && detail.post_commit_progress) {
            const pc = detail.post_commit_progress;
            const pct = pc.progress_pct || 0;
            html += `
                <div class="info-item lifecycle-progress" style="grid-column: 1/-1; border-top: 1px solid var(--border-color); margin-top: 0.5rem; padding-top: 0.5rem;">
                    <span class="info-label">☁️ Upload</span>
                    <div class="info-value" style="display: flex; flex-direction: column; gap: 0.25rem;">
                        <div style="display: flex; align-items: center; gap: 0.5rem; flex-wrap: wrap;">
                            <span class="badge badge-uploading">uploading</span>
                            <span class="text-xs font-mono">
                                ${pc.bucket ? pc.bucket + ' (' + pc.mode + ')' : ''}
                                | ${this.formatBytes(pc.bytes_sent)} / ${this.formatBytes(pc.total_bytes)}
                            </span>
                        </div>
                        <div class="progress-bar-wrap" style="margin-top: 0.25rem;">
                            <div class="progress-bar-fill" style="width: ${Math.min(pct, 100)}%; background: var(--color-orange, #f97316);"></div>
                        </div>
                        <span class="progress-text" style="font-size: 0.75rem;">${pct.toFixed(1)}%</span>
                    </div>
                </div>`;
        }

        // Done / Failed phase badge (sem progress bar, apenas status)
        if (detail.phase === 'done' || detail.phase === 'failed') {
            html += `
                <div class="info-item" style="grid-column: 1/-1;">
                    <span class="info-label">Phase</span>
                    <span class="info-value">${this.phaseBadge(detail.phase)}</span>
                </div>`;
        }

        return html;
    },

    // Badge de resultado para sessões finalizadas
    resultBadge(result) {
        const map = {
            ok: { cls: 'badge-success', label: '✓ ok' },
            checksum_mismatch: { cls: 'badge-error', label: '✗ checksum' },
            write_error: { cls: 'badge-warn', label: '⚠ write error' },
            timeout: { cls: 'badge-idle', label: '⏱ timeout' },
            error: { cls: 'badge-error', label: '✗ error' },
        };
        const m = map[result] || { cls: 'badge-neutral', label: result || '—' };
        return `<span class="badge ${m.cls}">${m.label}</span>`;
    },

    // Renderiza tabela de histórico de sessões finalizadas
    renderSessionHistory(entries) {
        const card = document.getElementById('session-history-card');
        const body = document.getElementById('session-history-body');

        if (!entries || entries.length === 0) {
            card.style.display = 'none';
            return;
        }

        card.style.display = '';
        body.innerHTML = entries.map(e => `
            <tr>
                <td><strong>${this.escapeHtml(e.agent)}</strong></td>
                <td>${this.escapeHtml(e.storage)}</td>
                <td>${this.modeBadge(e.mode)}</td>
                <td>${this.compressionBadge(e.compression)}</td>
                <td>${this.resultBadge(e.result)}</td>
                <td>${this.escapeHtml(e.duration)}</td>
                <td>${this.formatBytes(e.bytes_total)}</td>
                <td>${this.formatDateTime(e.finished_at)}</td>
            </tr>
        `).join('');
    },
    // Exporta eventos carregados como JSON ou CSV
    exportEvents(events, format = 'json') {
        if (!events || events.length === 0) return;

        let content, mimeType, ext;
        if (format === 'csv') {
            const headers = ['timestamp', 'level', 'type', 'agent', 'message'];
            const rows = events.map(e =>
                headers.map(h => '"' + String(e[h] || '').replace(/"/g, '""') + '"').join(',')
            );
            content = [headers.join(','), ...rows].join('\n');
            mimeType = 'text/csv';
            ext = 'csv';
        } else {
            content = JSON.stringify(events, null, 2);
            mimeType = 'application/json';
            ext = 'json';
        }

        const blob = new Blob([content], { type: mimeType });
        const url = URL.createObjectURL(blob);
        const a = document.createElement('a');
        a.href = url;
        a.download = `nbackup-events-${new Date().toISOString().slice(0, 10)}.${ext}`;
        document.body.appendChild(a);
        a.click();
        document.body.removeChild(a);
        URL.revokeObjectURL(url);
    },

    // Renderiza o card global "Chunk Buffer" no overview.
    // Exibido apenas quando buf != null. Se buf.enabled=false, exibe estado desabilitado.
    renderChunkBufferCard(buf) {
        const el = document.getElementById('chunk-buffer-card');
        if (!el) return;

        if (!buf) {
            el.style.display = 'none';
            return;
        }

        el.style.display = '';

        if (!buf.enabled) {
            el.innerHTML = `
                <div class="chunk-buffer-header">
                    <span class="chunk-buffer-title">Chunk Buffer</span>
                    <span class="badge badge-neutral">DESABILITADO</span>
                </div>
                <p style="font-size:0.8rem;color:var(--text-muted);">Buffer desabilitado (<code>size: 0</code>).</p>`;
            el.classList.add('disabled');
            return;
        }

        el.classList.remove('disabled');

        const fillPct = (buf.fill_ratio || 0) * 100;
        let fillColor = 'low';
        if (fillPct >= 80) fillColor = 'high';
        else if (fillPct >= 50) fillColor = 'med';

        const pending = Math.max(0, (buf.total_pushed || 0) - (buf.total_drained || 0));
        const bpBadge = (buf.backpressure_events || 0) > 0
            ? `<span class="badge badge-backpressure" title="Total desde o início do servidor">⚡ ${buf.backpressure_events} backpressure</span>`
            : '';

        el.innerHTML = `
            <div class="chunk-buffer-header">
                <span class="chunk-buffer-title">Chunk Buffer</span>
                <span class="badge badge-running">● ATIVO</span>
            </div>
            <div class="chunk-buffer-fill-wrap">
                <div style="display:flex;justify-content:space-between;font-size:0.7rem;color:var(--text-muted);margin-bottom:4px;">
                    <span>Em uso: ${this.formatBytes(buf.in_flight_bytes)} / ${this.formatBytes(buf.capacity_bytes)}</span>
                    <span>${fillPct.toFixed(1)}%</span>
                </div>
                <div class="stat-track">
                    <div class="stat-fill ${fillColor}" style="width:${Math.min(fillPct, 100)}%"></div>
                </div>
            </div>
            <div class="chunk-buffer-metrics">
                <div class="chunk-buffer-metric">
                    <span class="chunk-buffer-metric-label">Drain ratio</span>
                    <span class="chunk-buffer-metric-value">${buf.drain_ratio === 0 ? 'write-through' : buf.drain_ratio}</span>
                </div>
                <div class="chunk-buffer-metric">
                    <span class="chunk-buffer-metric-label">Drenagem</span>
                    <span class="chunk-buffer-metric-value">${(buf.drain_rate_mbs || 0).toFixed(1)} MB/s</span>
                </div>
                <div class="chunk-buffer-metric">
                    <span class="chunk-buffer-metric-label">Fallbacks</span>
                    <span class="chunk-buffer-metric-value">${buf.total_fallbacks || 0}</span>
                </div>
                <div class="chunk-buffer-metric">
                    <span class="chunk-buffer-metric-label">Pendentes</span>
                    <span class="chunk-buffer-metric-value">${pending}</span>
                </div>
            </div>
            ${bpBadge ? `<div style="margin-top:8px;">${bpBadge}</div>` : ''}
        `;
    },

    // Renderiza o card "Object Storage Sync" no overview.
    renderSyncStatusCard(sync) {
        const el = document.getElementById('sync-status-card');
        if (!el) return;

        if (!sync || (!sync.running && !sync.last_result)) {
            el.style.display = 'none';
            return;
        }

        el.style.display = '';

        let content = '';

        if (sync.running && sync.progress) {
            const p = sync.progress;
            const pct = p.progress_pct || 0;
            let barColor = 'low';
            if (pct >= 80) barColor = 'high';
            else if (pct >= 40) barColor = 'med';

            content = `
                <div class="sync-header">
                    <span class="sync-title">☁ Object Storage Sync</span>
                    <span class="badge badge-running sync-badge-pulse">● SYNCING</span>
                </div>
                <div class="sync-progress-section">
                    <div class="sync-progress-top">
                        <span class="sync-progress-label">${pct.toFixed(1)}% — ${p.processed_files}/${p.total_files} arquivos</span>
                        ${p.eta ? `<span class="sync-eta">ETA: ${p.eta}</span>` : ''}
                    </div>
                    <div class="stat-track">
                        <div class="stat-fill ${barColor} sync-progress-animated" style="width:${Math.min(pct, 100)}%"></div>
                    </div>
                    <div class="sync-current-file" title="${this.escapeHtml(p.current_file)}">
                        ${p.current_bucket ? `<span class="badge badge-neutral text-xs">${this.escapeHtml(p.current_bucket)}</span>` : ''}
                        <span class="sync-file-path">${this.escapeHtml(p.current_file || '...')}</span>
                    </div>
                </div>
                <div class="sync-counters">
                    <div class="sync-counter">
                        <span class="sync-counter-value">${p.uploaded_files}</span>
                        <span class="sync-counter-label">Uploaded</span>
                    </div>
                    <div class="sync-counter">
                        <span class="sync-counter-value">${p.skipped_files}</span>
                        <span class="sync-counter-label">Skipped</span>
                    </div>
                    <div class="sync-counter sync-counter-errors">
                        <span class="sync-counter-value">${p.error_files}</span>
                        <span class="sync-counter-label">Errors</span>
                    </div>
                    <div class="sync-counter">
                        <span class="sync-counter-value">${this.formatBytes(p.bytes_uploaded)}</span>
                        <span class="sync-counter-label">Bytes</span>
                    </div>
                </div>
                ${sync.elapsed ? `<div class="sync-elapsed">Tempo decorrido: ${sync.elapsed}</div>` : ''}
            `;
        } else if (sync.last_result) {
            const r = sync.last_result;
            const hasErrors = r.errors > 0;

            let bucketsTable = '';
            if (r.buckets && r.buckets.length > 0) {
                const rows = r.buckets.map(b => `
                    <tr>
                        <td>${this.escapeHtml(b.storage_name)}</td>
                        <td><span class="badge badge-neutral text-xs">${this.escapeHtml(b.bucket_name)}</span></td>
                        <td>${b.uploaded}</td>
                        <td>${b.skipped}</td>
                        <td>${b.errors > 0 ? `<span class="badge badge-error">${b.errors}</span>` : '0'}</td>
                        <td>${b.duration}</td>
                        <td>${b.error ? `<span class="badge badge-error" title="${this.escapeHtml(b.error)}">!</span>` : '—'}</td>
                    </tr>
                `).join('');
                bucketsTable = `
                    <div class="sync-buckets-table table-wrap">
                        <table>
                            <thead><tr>
                                <th>Storage</th><th>Bucket</th><th>↑</th><th>≡</th><th>✗</th><th>Duração</th><th>Erro</th>
                            </tr></thead>
                            <tbody>${rows}</tbody>
                        </table>
                    </div>
                `;
            }

            content = `
                <div class="sync-header">
                    <span class="sync-title">☁ Object Storage Sync</span>
                    <span class="badge ${hasErrors ? 'badge-warn' : 'badge-success'}">${hasErrors ? '⚠ CONCLUÍDO COM ERROS' : '✓ CONCLUÍDO'}</span>
                </div>
                <div class="sync-result-summary">
                    <div class="sync-counter">
                        <span class="sync-counter-value">${r.uploaded}</span>
                        <span class="sync-counter-label">Uploaded</span>
                    </div>
                    <div class="sync-counter">
                        <span class="sync-counter-value">${r.skipped}</span>
                        <span class="sync-counter-label">Skipped</span>
                    </div>
                    <div class="sync-counter sync-counter-errors">
                        <span class="sync-counter-value">${r.errors}</span>
                        <span class="sync-counter-label">Errors</span>
                    </div>
                    <div class="sync-counter">
                        <span class="sync-counter-value">${r.duration}</span>
                        <span class="sync-counter-label">Duração</span>
                    </div>
                </div>
                <div class="sync-result-times">
                    <span>Início: ${this.formatDateTime(r.started_at)}</span>
                    <span>Fim: ${this.formatDateTime(r.ended_at)}</span>
                </div>
                ${bucketsTable}
            `;
        }

        el.innerHTML = content;
    },

    // Renderiza linha compacta de buffer inline nas sessões ativas (na lista).
    // Chamado apenas quando session.buffer_in_flight_bytes > 0.
    renderBufferSessionInline(session) {
        const pct = Math.min(session.buffer_fill_percent || 0, 100);
        let fillColor = 'low';
        if (pct >= 80) fillColor = 'high';
        else if (pct >= 50) fillColor = 'med';

        return `
            <div class="buffer-inline">
                <span style="font-size:0.65rem;text-transform:uppercase;letter-spacing:0.06em;color:var(--text-muted);">Buffer</span>
                <div class="stat-track">
                    <div class="stat-fill ${fillColor}" style="width:${pct}%"></div>
                </div>
                <span>${this.formatBytes(session.buffer_in_flight_bytes)} (${pct.toFixed(1)}%)</span>
            </div>`;
    },

    // Renderiza bloco "Buffer de Memória" no detalhe da sessão.
    // Chamado sempre que detail.buffer_enabled == true.
    renderBufferMemoryBlock(detail) {
        const pct = (detail.buffer_fill_percent || 0).toFixed(1);
        const bufMB = ((detail.buffer_in_flight_bytes || 0) / (1024 * 1024)).toFixed(2);
        return `
            <div class="info-item" style="grid-column:1/-1;">
                <span class="info-label">Buffer de Memória</span>
                <div style="display:flex;flex-direction:column;gap:8px;margin-top:6px;">
                    <div class="sparkline-card" style="padding:10px 14px;">
                        <div class="sparkline-header">
                            <span class="sparkline-label">Bytes em buffer</span>
                            <span class="sparkline-value" id="buf-value">${bufMB} MB (${pct}%)</span>
                        </div>
                        <canvas class="sparkline-canvas" id="spark-buffer" width="600" height="50"></canvas>
                    </div>
                </div>
            </div>`;
    },
};
