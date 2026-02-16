// api.js — Fetch wrapper para a API de observabilidade do NBackup

const API = {
    base: '',  // same origin

    async get(path) {
        const res = await fetch(`${this.base}${path}`);
        if (!res.ok) {
            throw new Error(`API ${res.status}: ${res.statusText}`);
        }
        return res.json();
    },

    health() { return this.get('/api/v1/health'); },
    metrics() { return this.get('/api/v1/metrics'); },
    sessions() { return this.get('/api/v1/sessions'); },
    session(id) { return this.get(`/api/v1/sessions/${encodeURIComponent(id)}`); },
    events(limit = 50) { return this.get(`/api/v1/events?limit=${limit}`); },
    config() { return this.get('/api/v1/config/effective'); },
    agents() { return this.get('/api/v1/agents'); },
};
