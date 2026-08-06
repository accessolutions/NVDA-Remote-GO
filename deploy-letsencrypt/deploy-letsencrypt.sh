#!/bin/sh
# =============================================================================
# deploy-letsencrypt.sh
# -----------------------------------------------------------------------------
# Bascule le serveur NVDA Remote (conteneur Docker) sur un certificat TLS
# signe par Let's Encrypt pour le domaine, au lieu du certificat auto-signe.
#
# METHODE REELLEMENT UTILISEE sur le serveur de production (sd-david) :
#   - Un serveur web (Apache) ecoute deja sur le port 80 de l'hote et sert le
#     DocumentRoot /var/www/html pour le domaine.
#   - On y publie le challenge HTTP-01 de Let's Encrypt (webroot), avec une
#     exception d'authentification limitee au chemin .well-known/acme-challenge.
#   - certbot (conteneur jetable) obtient le certificat, stocke dans un volume
#     Docker "le-etc".
#   - Le conteneur nvdaRemoteServer est recree en montant ce volume et en
#     utilisant fullchain.pem / privkey.pem.
#   - Une tache cron utilisateur renouvelle automatiquement (voir renew.sh).
#
# Le port 80 n'a AUCUN impact sur l'acces distant NVDA Remote : il sert
# uniquement a prouver que le domaine est connu et a faire signer le certificat.
#
# Prerequis :
#   - Docker installe, l'utilisateur courant est dans le groupe docker (pas de
#     root necessaire ; le demon Docker ecrit dans le webroot via un conteneur).
#   - Un serveur web ecoute sur le port 80 pour le domaine, DocumentRoot connu.
#   - DNS de DOMAIN pointant vers l'IP publique du serveur.
#   - Ports 80 et 6837 (et 443 si -ws-address :443) ouverts.
#
# Usage : ./deploy-letsencrypt.sh
# =============================================================================
set -eu

# --------------------------- Parametres (a adapter) --------------------------
DOMAIN="nvdaremote.accessolutions.fr"
EMAIL="contact@accessolutions.fr"

# DocumentRoot du vhost qui sert DOMAIN sur le port 80 (webroot du challenge).
WEBROOT="/var/www/html"

# Conteneur NVDA Remote existant.
SERVER_NAME="nvdaremote"
SERVER_IMAGE="nvdaremoteserver-docker"
# Arguments de lancement du serveur (hors cert/key, ajoutes automatiquement).
# Ici : WebSocket securise sur 443 en plus du TLS brut sur 6837.
SERVER_ARGS="-conf-read=false -ws-address :443"
# Ports publies par le conteneur (format -p de docker run).
SERVER_PORTS="-p 443:443 -p 6837:6837"

# Volumes Docker pour certbot.
VOL_ETC="le-etc"          # /etc/letsencrypt (certificats + config)
VOL_LIB="le-lib"          # /var/lib/letsencrypt (etat interne certbot)

CERT="/etc/letsencrypt/live/${DOMAIN}/fullchain.pem"
KEY="/etc/letsencrypt/live/${DOMAIN}/privkey.pem"

# Mettre STAGING=1 pour un test a blanc (aucun quota consomme).
STAGING=0

log() { echo ">>> $*"; }

require_docker() {
    command -v docker >/dev/null 2>&1 || { echo "ERREUR : Docker absent." >&2; exit 1; }
}

# --- 1. Exception ACME dans le webroot (via conteneur, droits du demon) -------
prepare_webroot() {
    log "Preparation du chemin ACME dans ${WEBROOT}/.well-known/acme-challenge"
    docker run --rm -v "${WEBROOT}:/wwwroot" alpine sh -c '
        mkdir -p /wwwroot/.well-known/acme-challenge &&
        printf "Require all granted\nSatisfy any\n" > /wwwroot/.well-known/acme-challenge/.htaccess &&
        chmod -R a+rX /wwwroot/.well-known'
    # Verification externe.
    log "Verification de l acces public au challenge"
    docker run --rm -v "${WEBROOT}:/wwwroot" alpine sh -c 'echo acme-ok > /wwwroot/.well-known/acme-challenge/_test'
    code=$(curl -sS -o /dev/null -w '%{http_code}' "http://${DOMAIN}/.well-known/acme-challenge/_test" || echo 000)
    docker run --rm -v "${WEBROOT}:/wwwroot" alpine rm -f /wwwroot/.well-known/acme-challenge/_test
    if [ "${code}" != "200" ]; then
        echo "ERREUR : le challenge n'est pas servi publiquement (HTTP ${code})." >&2
        echo "Verifiez le DocumentRoot du vhost servant ${DOMAIN} et AllowOverride." >&2
        exit 1
    fi
    log "Challenge accessible (HTTP 200)."
}

# --- 2. Emission du certificat ------------------------------------------------
obtain_cert() {
    staging=""
    [ "${STAGING}" = "1" ] && { log "MODE STAGING (test)"; staging="--staging"; }

    if docker run --rm -v "${VOL_ETC}:/etc/letsencrypt" alpine test -f "${CERT}" 2>/dev/null; then
        log "Certificat deja present, emission ignoree (utiliser renew.sh)."
        return 0
    fi
    log "Emission du certificat Let's Encrypt pour ${DOMAIN}"
    docker run --rm \
        -v "${VOL_ETC}:/etc/letsencrypt" \
        -v "${VOL_LIB}:/var/lib/letsencrypt" \
        -v "${WEBROOT}:${WEBROOT}" \
        certbot/certbot certonly --webroot -w "${WEBROOT}" \
            -d "${DOMAIN}" \
            --email "${EMAIL}" --agree-tos --no-eff-email --non-interactive \
            ${staging}
}

# --- 3. (Re)creation du conteneur NVDA Remote avec le cert signe ---------------
run_server() {
    log "Recreation du conteneur ${SERVER_NAME} avec le certificat signe"
    docker rm -f "${SERVER_NAME}" >/dev/null 2>&1 || true
    # shellcheck disable=SC2086
    docker run -d \
        --name "${SERVER_NAME}" \
        --restart unless-stopped \
        ${SERVER_PORTS} \
        -v "${VOL_ETC}:/etc/letsencrypt:ro" \
        "${SERVER_IMAGE}" \
        /nvdaRemoteServer ${SERVER_ARGS} -cert-file "${CERT}" -key-file "${KEY}"
}

# --- 4. Cron de renouvellement (utilisateur, sans root) -----------------------
install_cron() {
    SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
    line="17 3,15 * * * ${SCRIPT_DIR}/renew.sh >> ${HOME}/nvdaremote-renew.log 2>&1"
    log "Installation de la tache cron de renouvellement (2x/jour)"
    ( crontab -l 2>/dev/null | grep -vF "${SCRIPT_DIR}/renew.sh" ; echo "${line}" ) | crontab -
    echo "    ${line}"
}

require_docker
prepare_webroot
obtain_cert
run_server
install_cron

log "Termine. Verifiez :"
log "  echo | openssl s_client -connect ${DOMAIN}:6837 | openssl x509 -noout -issuer -dates"
log "Le certificat Let's Encrypt dure 90 jours et se renouvelle automatiquement."
