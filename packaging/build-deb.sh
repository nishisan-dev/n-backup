#!/usr/bin/env bash
# build-deb.sh — Gera pacotes .deb para nbackup-agent e nbackup-server
#
# Uso: ./packaging/build-deb.sh <VERSION> <ARCH>
#   VERSION: versão semântica (ex: 1.0.0)
#   ARCH:    arquitetura alvo (amd64 ou arm64)
#
# Exemplo:
#   ./packaging/build-deb.sh 1.0.0 amd64
#   ./packaging/build-deb.sh 1.0.0 arm64

set -euo pipefail

VERSION="${1:?Uso: $0 <VERSION> <ARCH>}"
ARCH="${2:?Uso: $0 <VERSION> <ARCH>}"

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
PROJECT_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
DIST_DIR="${PROJECT_ROOT}/dist"

# Mapear ARCH para GOARCH
case "${ARCH}" in
    amd64) GOARCH="amd64" ;;
    arm64) GOARCH="arm64" ;;
    *) echo "Arquitetura inválida: ${ARCH} (use amd64 ou arm64)"; exit 1 ;;
esac

echo "==> Compilando binários para ${ARCH}..."
export GOOS=linux
export GOARCH="${GOARCH}"
export CGO_ENABLED=0
LDFLAGS="-s -w -X github.com/nishisan-dev/n-backup/internal/server/observability.Version=${VERSION}"

mkdir -p "${DIST_DIR}"
go build -ldflags "${LDFLAGS}" -o "${DIST_DIR}/nbackup-agent" "${PROJECT_ROOT}/cmd/nbackup-agent"
go build -ldflags "${LDFLAGS}" -o "${DIST_DIR}/nbackup-server" "${PROJECT_ROOT}/cmd/nbackup-server"

# --- Função para montar um pacote .deb ---
build_deb() {
    local COMPONENT="$1"  # nbackup-agent ou nbackup-server
    local CONFIG_FILE="$2" # agent.example.yaml ou server.example.yaml
    local CONFIG_DEST="$3" # agent.yaml ou server.yaml

    echo "==> Montando pacote ${COMPONENT}..."

    local PKG_DIR="${DIST_DIR}/${COMPONENT}_${VERSION}_${ARCH}"
    rm -rf "${PKG_DIR}"

    # --- DEBIAN ---
    mkdir -p "${PKG_DIR}/DEBIAN"
    cp "${SCRIPT_DIR}/deb/${COMPONENT}/DEBIAN/"* "${PKG_DIR}/DEBIAN/"
    chmod 755 "${PKG_DIR}/DEBIAN/postinst" "${PKG_DIR}/DEBIAN/prerm"

    # Substituir placeholders
    sed -i "s/VERSION_PLACEHOLDER/${VERSION}/g" "${PKG_DIR}/DEBIAN/control"
    sed -i "s/ARCH_PLACEHOLDER/${ARCH}/g" "${PKG_DIR}/DEBIAN/control"

    # --- Binário ---
    mkdir -p "${PKG_DIR}/usr/bin"
    cp "${DIST_DIR}/${COMPONENT}" "${PKG_DIR}/usr/bin/${COMPONENT}"
    chmod 755 "${PKG_DIR}/usr/bin/${COMPONENT}"

    # --- Config exemplo ---
    mkdir -p "${PKG_DIR}/etc/nbackup"
    cp "${PROJECT_ROOT}/configs/${CONFIG_FILE}" "${PKG_DIR}/etc/nbackup/${CONFIG_DEST}"

    # --- Systemd unit ---
    mkdir -p "${PKG_DIR}/lib/systemd/system"
    cp "${SCRIPT_DIR}/systemd/${COMPONENT}.service" "${PKG_DIR}/lib/systemd/system/"

    # --- Man page ---
    mkdir -p "${PKG_DIR}/usr/share/man/man1"
    local MAN_FILE="${SCRIPT_DIR}/man/${COMPONENT}.1"
    # Substituir versão na man page
    sed "s/VERSION_PLACEHOLDER/${VERSION}/g" "${MAN_FILE}" | gzip -9 > "${PKG_DIR}/usr/share/man/man1/${COMPONENT}.1.gz"

    # --- Documentação ---
    mkdir -p "${PKG_DIR}/usr/share/doc/${COMPONENT}"
    cp "${PROJECT_ROOT}/LICENSE" "${PKG_DIR}/usr/share/doc/${COMPONENT}/"
    cp "${PROJECT_ROOT}/README.md" "${PKG_DIR}/usr/share/doc/${COMPONENT}/"

    # --- Build .deb ---
    echo "==> Gerando ${COMPONENT}_${VERSION}_${ARCH}.deb..."
    dpkg-deb --build --root-owner-group "${PKG_DIR}" "${DIST_DIR}/${COMPONENT}_${VERSION}_${ARCH}.deb"

    # Limpar diretório temporário
    rm -rf "${PKG_DIR}"

    echo "==> ${COMPONENT}_${VERSION}_${ARCH}.deb gerado com sucesso."
}

# --- Montar ambos os pacotes ---
build_deb "nbackup-agent" "agent.example.yaml" "agent.yaml"
build_deb "nbackup-server" "server.example.yaml" "server.yaml"

# --- Limpar binários soltos ---
rm -f "${DIST_DIR}/nbackup-agent" "${DIST_DIR}/nbackup-server"

# --- Checksums ---
echo "==> Gerando checksums..."
cd "${DIST_DIR}"
sha256sum nbackup-*.deb > "checksums-deb-${ARCH}.txt"

echo ""
echo "==> Pacotes gerados em ${DIST_DIR}/:"
ls -lh "${DIST_DIR}/"*.deb
echo ""
cat "${DIST_DIR}/checksums-deb-${ARCH}.txt"
