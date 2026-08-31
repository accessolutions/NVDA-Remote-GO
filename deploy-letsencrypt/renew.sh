#!/bin/sh
# =============================================================================
# renew.sh
# -----------------------------------------------------------------------------
# Renouvelle le certificat Let's Encrypt du domaine si necessaire, puis
# redemarre le conteneur du serveur uniquement en cas de renouvellement
# effectif (le serveur Go lit le certificat au demarrage).
#
# Les parametres sont repris du fichier .env place a cote de ce script, comme
# pour deploy-letsencrypt.sh. Aucun secret n'est stocke ici.
#
# Appele par cron (voir deploy-letsencrypt.sh, 2x/jour).
#
# certbot ne renouvelle reellement qu'a ~30 jours de l'expiration
# (validite totale : 90 jours). Ne necessite pas root (groupe docker).
# =============================================================================
set -eu

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
if [ -f "${SCRIPT_DIR}/.env" ]; then
    # shellcheck disable=SC1091
    . "${SCRIPT_DIR}/.env"
fi

SERVER_NAME="${SERVER_NAME:-nvdaremote}"
WEBROOT="${WEBROOT:-/var/www/html}"
VOL_ETC="${VOL_ETC:-le-etc}"
VOL_LIB="${VOL_LIB:-le-lib}"
FLAG="/etc/letsencrypt/nvdaremote.renewed"

docker run --rm \
  -v "${VOL_ETC}:/etc/letsencrypt" \
  -v "${VOL_LIB}:/var/lib/letsencrypt" \
  -v "${WEBROOT}:${WEBROOT}" \
  certbot/certbot renew --webroot -w "${WEBROOT}" \
  --deploy-hook "touch ${FLAG}"

if docker run --rm -v "${VOL_ETC}:/etc/letsencrypt" alpine test -f "${FLAG}"; then
  echo "$(date '+%F %T') Certificat renouvele -> redemarrage ${SERVER_NAME}"
  docker restart "${SERVER_NAME}"
  docker run --rm -v "${VOL_ETC}:/etc/letsencrypt" alpine rm -f "${FLAG}"
else
  echo "$(date '+%F %T') Aucun renouvellement necessaire"
fi
