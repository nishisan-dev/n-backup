// api.js — Fetch wrapper para a API de observabilidade do NBackup

const API = {
    base: '',  // same origin

    async get(path, init = {}) {
        const res = await fetch(`${this.base}${path}`, init);
        if (!res.ok) {
            const err = new Error(`API ${res.status}: ${res.statusText}`);
            err.status = res.status;
            err.path = path;
            throw err;
        }
        return res.json();
    },

    health(init) { return this.get('/api/v1/health', init); },
    metrics(init) { return this.get('/api/v1/metrics', init); },
    sessions(init) { return this.get('/api/v1/sessions', init); },
    session(id, init) { return this.get(`/api/v1/sessions/${encodeURIComponent(id)}`, init); },
    events(limit = 50, init) { return this.get(`/api/v1/events?limit=${limit}`, init); },
    config(init) { return this.get('/api/v1/config/effective', init); },
    agents(init) { return this.get('/api/v1/agents', init); },
    storages(init) { return this.get('/api/v1/storages', init); },
    sessionsHistory(init) { return this.get('/api/v1/sessions/history', init); },
    activeSessionsHistory(limit = 120, sessionId = "", init) {
        const sid = sessionId ? `&session_id=${encodeURIComponent(sessionId)}` : "";
        return this.get(`/api/v1/sessions/active-history?limit=${limit}${sid}`, init);
    },
};
