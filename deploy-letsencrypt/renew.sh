#!/bin/sh
# =============================================================================
# renew.sh
# -----------------------------------------------------------------------------
# Renouvelle le certificat Let's Encrypt de nvdaremote.accessolutions.fr et
# remote.accessolutions.fr si necessaire,
# puis redemarre le conteneur nvdaremote uniquement en cas de renouvellement
# effectif (le serveur Go lit le certificat au demarrage).
#
# Version identique a /home/accesso/nvdaremote-renew.sh installee sur le
# serveur. Appelee par cron (voir deploy-letsencrypt.sh, 2x/jour).
#
# certbot ne renouvelle reellement qu'a ~30 jours de l'expiration
# (validite totale : 90 jours). Ne necessite pas root (groupe docker).
# =============================================================================
set -eu

SERVER_NAME="nvdaremote"
WEBROOT="/var/www/html"
VOL_ETC="le-etc"
VOL_LIB="le-lib"
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
