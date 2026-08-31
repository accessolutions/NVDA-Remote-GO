#!/bin/sh
# =============================================================================
# deploy-letsencrypt.sh
# -----------------------------------------------------------------------------
# Bascule le serveur NVDA Remote (conteneur Docker) sur un certificat TLS
# signe par Let's Encrypt pour le domaine, au lieu du certificat auto-signe.
#
# METHODE REELLEMENT UTILISEE en production :
#   - Un serveur web ecoute deja sur le port 80 de l'hote et sert le
#     DocumentRoot pour le domaine.
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
#   - DNS de DOMAIN et ADDITIONAL_DOMAIN pointant vers l'IP publique du serveur.
#   - Ports 80 et 6837 (et 443 si -ws-address :443) ouverts.
#
# Usage : ./deploy-letsencrypt.sh
# =============================================================================
set -eu

# --------------------------- Parametres --------------------------------------
# Toutes les valeurs ci-dessous peuvent etre surchargees, sans modifier ce
# script, par un fichier .env place a cote de lui. Copiez .env.example en .env
# et adaptez-le. Le fichier .env n'est jamais versionne : c'est la qu'il faut
# mettre tout ce qui est propre a votre installation, et tout ce qui est
# sensible. Ce script ne contient volontairement aucun secret.
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
if [ -f "${SCRIPT_DIR}/.env" ]; then
    # shellcheck disable=SC1091
    . "${SCRIPT_DIR}/.env"
fi

DOMAIN="${DOMAIN:-nvdaremote.example.com}"
ADDITIONAL_DOMAIN="${ADDITIONAL_DOMAIN:-remote.example.com}"
EMAIL="${EMAIL:-contact@example.com}"

# DocumentRoot du vhost qui sert les deux domaines sur le port 80 (webroot du challenge).
WEBROOT="${WEBROOT:-/var/www/html}"

# Conteneur NVDA Remote existant.
SERVER_NAME="${SERVER_NAME:-nvdaremote}"
SERVER_IMAGE="${SERVER_IMAGE:-nvdaremoteserver-docker}"
# Arguments de lancement du serveur (hors cert/key, ajoutes automatiquement).
# Ici : WebSocket securise sur 443, qui accepte aussi le protocole historique
# grace au multiplexage, en plus du TLS historique sur 6837.
#
# Le partage d'ecran exige en plus -screen-share et au moins un -turn-url ;
# check_screen_share() ci-dessous refuse de deployer une configuration
# incoherente, car elle donnerait un partage d'ecran qui negocie puis echoue.
SERVER_ARGS="${SERVER_ARGS:--conf-read=false -ws-address :443}"
# Ports publies par le conteneur (format -p de docker run).
SERVER_PORTS="${SERVER_PORTS:--p 443:443 -p 6837:6837}"
# Fichier de l'hote contenant le secret partage avec le serveur TURN. Il est
# monte en lecture seule au meme chemin dans le conteneur.
TURN_SECRET_FILE="${TURN_SECRET_FILE:-}"

# Volumes Docker pour certbot.
VOL_ETC="${VOL_ETC:-le-etc}"          # /etc/letsencrypt (certificats + config)
VOL_LIB="${VOL_LIB:-le-lib}"          # /var/lib/letsencrypt (etat interne certbot)

CERT="/etc/letsencrypt/live/${DOMAIN}/fullchain.pem"
KEY="/etc/letsencrypt/live/${DOMAIN}/privkey.pem"
CERT_NAME="${DOMAIN}"

# Mettre STAGING=1 pour un test a blanc (aucun quota consomme).
STAGING="${STAGING:-0}"

log() { echo ">>> $*"; }

require_docker() {
    command -v docker >/dev/null 2>&1 || { echo "ERREUR : Docker absent." >&2; exit 1; }
}

# --- 0. Coherence des options de partage d'ecran ------------------------------
# Le serveur ne relaie la signalisation WebRTC que si -screen-share est present,
# et ne repond a la demande turn_credentials que s'il connait au moins une URL
# ICE. Une configuration incomplete ne produit aucune erreur au demarrage : la
# session se negocie normalement puis la connexion video n'aboutit jamais des
# que les deux postes ne sont pas sur le meme reseau. Le probleme est donc
# detecte ici, ou il est encore lisible.

# Vrai lorsque SERVER_ARGS active l'option nommee. "-opt=false" ne compte pas.
server_arg_enabled() {
    for arg in ${SERVER_ARGS}; do
        case "${arg}" in
            "-$1"|"--$1")
                return 0
                ;;
            "-$1="*|"--$1="*)
                case "${arg#*=}" in
                    false|0|no) return 1 ;;
                    *) return 0 ;;
                esac
                ;;
        esac
    done
    return 1
}

# Ecrit une URL ICE par ligne, quelle que soit la forme employee.
server_turn_urls() {
    pending=0
    for arg in ${SERVER_ARGS}; do
        if [ "${pending}" = "1" ]; then
            echo "${arg}"
            pending=0
            continue
        fi
        case "${arg}" in
            -turn-url|--turn-url) pending=1 ;;
            -turn-url=*|--turn-url=*) echo "${arg#*=}" ;;
        esac
    done
}

check_screen_share() {
    if ! server_arg_enabled screen-share; then
        if [ -n "$(server_turn_urls)" ]; then
            echo "AVERTISSEMENT : des URL TURN sont configurees mais -screen-share est absent de SERVER_ARGS ; le serveur les ignorera." >&2
        fi
        return 0
    fi
    urls="$(server_turn_urls)"
    if [ -z "${urls}" ]; then
        echo "ERREUR : SERVER_ARGS active -screen-share sans aucun -turn-url." >&2
        echo "Le serveur ne repondrait pas aux demandes turn_credentials, et le partage d'ecran" >&2
        echo "echouerait des que les deux postes ne sont pas sur le meme reseau." >&2
        echo "Ajoutez au moins un -turn-url stun:... ou turns:... dans .env." >&2
        exit 1
    fi
    needs_secret=0
    for url in ${urls}; do
        case "${url}" in
            turn:*|turns:*) needs_secret=1 ;;
            stun:*|stuns:*) ;;
            *)
                echo "ERREUR : l URL ICE ${url} doit commencer par stun:, stuns:, turn: ou turns:." >&2
                exit 1
                ;;
        esac
    done
    if [ "${needs_secret}" = "0" ]; then
        return 0
    fi
    if ! server_arg_enabled turn-secret-file; then
        echo "ERREUR : une URL turn: ou turns: est configuree sans -turn-secret-file." >&2
        echo "Le serveur ne pourrait pas signer de justificatifs et n annoncerait que le STUN." >&2
        exit 1
    fi
    if [ -z "${TURN_SECRET_FILE}" ]; then
        echo "ERREUR : renseignez TURN_SECRET_FILE dans .env pour que le secret soit monte dans le conteneur." >&2
        exit 1
    fi
    if [ ! -r "${TURN_SECRET_FILE}" ]; then
        echo "ERREUR : le fichier de secret TURN ${TURN_SECRET_FILE} est introuvable ou illisible." >&2
        exit 1
    fi
    case " ${SERVER_ARGS} " in
        *" ${TURN_SECRET_FILE} "*|*"=${TURN_SECRET_FILE} "*) ;;
        *)
            echo "ERREUR : -turn-secret-file doit designer ${TURN_SECRET_FILE}, chemin monte dans le conteneur." >&2
            exit 1
            ;;
    esac
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
    for domain in "${DOMAIN}" "${ADDITIONAL_DOMAIN}"; do
        code=$(curl -sS -o /dev/null -w '%{http_code}' "http://${domain}/.well-known/acme-challenge/_test" || echo 000)
        if [ "${code}" != "200" ]; then
            echo "ERREUR : le challenge n'est pas servi publiquement pour ${domain} (HTTP ${code})." >&2
            echo "Verifiez le DocumentRoot du vhost servant ${domain} et AllowOverride." >&2
            docker run --rm -v "${WEBROOT}:/wwwroot" alpine rm -f /wwwroot/.well-known/acme-challenge/_test
            exit 1
        fi
    done
    docker run --rm -v "${WEBROOT}:/wwwroot" alpine rm -f /wwwroot/.well-known/acme-challenge/_test
    log "Challenge accessible pour les deux domaines (HTTP 200)."
}

# --- 2. Emission du certificat ------------------------------------------------
obtain_cert() {
    staging=""
    [ "${STAGING}" = "1" ] && { log "MODE STAGING (test)"; staging="--staging"; }

    expand=""
    if docker run --rm -v "${VOL_ETC}:/etc/letsencrypt" alpine test -f "${CERT}" 2>/dev/null; then
        expand="--expand"
        log "Extension du certificat Let's Encrypt avec ${ADDITIONAL_DOMAIN}"
    else
        log "Emission du certificat Let's Encrypt pour ${DOMAIN} et ${ADDITIONAL_DOMAIN}"
    fi
    docker run --rm \
        -v "${VOL_ETC}:/etc/letsencrypt" \
        -v "${VOL_LIB}:/var/lib/letsencrypt" \
        -v "${WEBROOT}:${WEBROOT}" \
        certbot/certbot certonly --webroot -w "${WEBROOT}" \
            --cert-name "${CERT_NAME}" ${expand} \
            -d "${DOMAIN}" \
            -d "${ADDITIONAL_DOMAIN}" \
            --email "${EMAIL}" --agree-tos --no-eff-email --non-interactive \
            ${staging}
}

# --- 3. (Re)creation du conteneur NVDA Remote avec le cert signe ---------------
run_server() {
    log "Recreation du conteneur ${SERVER_NAME} avec le certificat signe"
    secret_mount=""
    if [ -n "${TURN_SECRET_FILE}" ]; then
        secret_mount="-v ${TURN_SECRET_FILE}:${TURN_SECRET_FILE}:ro"
    fi
    docker rm -f "${SERVER_NAME}" >/dev/null 2>&1 || true
    # shellcheck disable=SC2086
    docker run -d \
        --name "${SERVER_NAME}" \
        --restart unless-stopped \
        ${SERVER_PORTS} \
        -v "${VOL_ETC}:/etc/letsencrypt:ro" \
        ${secret_mount} \
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
check_screen_share
prepare_webroot
obtain_cert
run_server
install_cron

log "Termine. Verifiez :"
log "  echo | openssl s_client -connect ${DOMAIN}:6837 | openssl x509 -noout -issuer -dates"
log "  echo | openssl s_client -connect ${ADDITIONAL_DOMAIN}:6837 -servername ${ADDITIONAL_DOMAIN} | openssl x509 -noout -issuer -dates"
log "Le certificat Let's Encrypt dure 90 jours et se renouvelle automatiquement."
