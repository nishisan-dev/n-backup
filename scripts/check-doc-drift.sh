#!/usr/bin/env bash
# scripts/check-doc-drift.sh
# Valida invariantes estruturais entre docs/ (fonte de verdade) e wiki/ (espelho).
# Exit 0 = tudo ok. Exit 1 = violações encontradas.

set -euo pipefail

VIOLATIONS=0
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

fail() {
  echo "  ❌ $*" >&2
  VIOLATIONS=$((VIOLATIONS + 1))
}

check() {
  local desc="$1"; local file="$2"; local pattern="$3"
  if ! grep -q "$pattern" "$ROOT/$file" 2>/dev/null; then
    fail "$file: padrão ausente: $pattern"
  fi
}

check_absent() {
  local desc="$1"; local file="$2"; local pattern="$3"
  if grep -q "$pattern" "$ROOT/$file" 2>/dev/null; then
    fail "$file: padrão obsoleto/inválido encontrado: $pattern"
  fi
}

echo "=== check-doc-drift.sh ==="
echo ""

# ─── 1. Valores de limite validados no código ────────────────────────────────
echo "1. Limites/ranges (código: internal/config/agent.go)"

for f in docs/usage.md docs/specification.md wiki/Guia-de-Uso.md wiki/Especificacao-Tecnica.md; do
  check_absent "range inválido" "$f" "1-8"
done

for f in docs/usage.md wiki/Guia-de-Uso.md; do
  check "range correto parallels" "$f" "1-255"
done

for f in docs/specification.md wiki/Especificacao-Tecnica.md; do
  check "range correto MaxStreams/parallels" "$f" "1-255"
done

# ─── 2. Schema de configuração ───────────────────────────────────────────────
echo "2. Schema de configuração"

# storages: (plural) — nunca storage: (singular em raiz de config)
check_absent "schema storage singular" "docs/specification.md" "^storage:"
check_absent "schema storage singular" "wiki/Especificacao-Tecnica.md" "^storage:"

# schedule está em backups[], não direto em daemon:
# Detecta o padrão do erro: "daemon:\n  schedule:" sem "control_channel:" intermediário
if grep -A1 "^daemon:" "$ROOT/docs/usage.md" 2>/dev/null | grep -q "^\s*schedule:"; then
  fail "docs/usage.md: 'daemon.schedule' encontrado — schedule deve estar em backups[].schedule"
fi

# ─── 3. Frames do protocolo ─────────────────────────────────────────────────
echo "3. Frames do protocolo (CIDN obrigatório nos dois lados)"

check "CIDN em docs/specification"       "docs/specification.md"          "CIDN"
check "CIDN em wiki/Especificação"       "wiki/Especificacao-Tecnica.md"  "CIDN"

# ─── 4. Features novas em ambos os lados ────────────────────────────────────
echo "4. Features novas cobertas nos dois lados"

for feature in "chunk_buffer" "dscp\|DSCP"; do
  check "feature em docs/usage.md"       "docs/usage.md"        "$feature"
  check "feature em wiki/Guia-de-Uso.md" "wiki/Guia-de-Uso.md"  "$feature"
done

check "chunk_shard_levels em docs"  "docs/specification.md"         "chunk_shard_levels"
check "chunk_shard_levels em wiki"  "wiki/Especificacao-Tecnica.md" "chunk_shard_levels"

# ─── 5. Seções-chave existem em ambos os lados ───────────────────────────────
echo "5. Seções-chave espelhadas (docs/ ↔ wiki/)"

declare -A PAIRS=(
  ["docs/usage.md"]="wiki/Guia-de-Uso.md"
  ["docs/specification.md"]="wiki/Especificacao-Tecnica.md"
  ["docs/architecture.md"]="wiki/Arquitetura.md"
  ["docs/installation.md"]="wiki/Instalacao.md"
)

for doc in "${!PAIRS[@]}"; do
  wiki="${PAIRS[$doc]}"
  doc_count=$(grep -c "^## " "$ROOT/$doc" 2>/dev/null || echo 0)
  wiki_count=$(grep -c "^## " "$ROOT/$wiki" 2>/dev/null || echo 0)
  if [ "$doc_count" -eq 0 ] || [ "$wiki_count" -eq 0 ]; then
    fail "Seções não encontradas: $doc ($doc_count) ou $wiki ($wiki_count)"
  fi
done

# ─── 6. README tem features obrigatórias ─────────────────────────────────────
echo "6. README.md — features obrigatórias"

for feature in "Chunk Buffer" "DSCP" "WebUI" "AutoScaler" "Bandwidth Throttling"; do
  check "README: $feature" "README.md" "$feature"
done

# ─── Resultado ───────────────────────────────────────────────────────────────
echo ""
if [ "$VIOLATIONS" -eq 0 ]; then
  echo "✅ All checks passed."
  exit 0
else
  echo "❌ $VIOLATIONS violação(ões) encontrada(s). Corrija antes de commitar." >&2
  exit 1
fi
