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
#
# Deux points a connaitre, verifies en production :
#
# 1. Le chemin du webroot monte dans le conteneur certbot doit etre celui
#    enregistre dans /etc/letsencrypt/renewal/<domaine>.conf (webroot_path et
#    webroot_map), sinon certbot cherche a demander interactivement le webroot
#    des domaines non mappes. L'option --non-interactive garantit un echec
#    explicite plutot qu'une attente sans fin.
#
# 2. En mode non interactif, certbot patiente un delai aleatoire pouvant
#    atteindre 8 minutes avant de tenter le renouvellement, afin de lisser la
#    charge sur Let's Encrypt. Ce n'est pas un blocage. Pour une verification
#    manuelle immediate, lancer avec NO_DELAY=1.
#
# Variables d'environnement acceptees :
#   DRY_RUN=1   essai a blanc, aucun certificat emis, aucun redemarrage
#   NO_DELAY=1  supprime le delai aleatoire (verification manuelle)
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
# Volume Docker contenant le webroot du defi HTTP-01. S'il est vide, le webroot
# est pris directement sur le systeme de fichiers de l'hote.
VOL_WEB="${VOL_WEB:-}"
FLAG="/etc/letsencrypt/nvdaremote.renewed"
DRY_RUN="${DRY_RUN:-0}"
NO_DELAY="${NO_DELAY:-0}"

if [ -n "${VOL_WEB}" ]; then
    webroot_mount="${VOL_WEB}:${WEBROOT}"
else
    webroot_mount="${WEBROOT}:${WEBROOT}"
fi

extra=""
if [ "${DRY_RUN}" = "1" ]; then
    echo "$(date '+%F %T') MODE ESSAI A BLANC (--dry-run)"
    extra="--dry-run"
fi
if [ "${NO_DELAY}" = "1" ]; then
    extra="${extra} --no-random-sleep-on-renew"
fi

# shellcheck disable=SC2086
docker run --rm \
  -v "${VOL_ETC}:/etc/letsencrypt" \
  -v "${VOL_LIB}:/var/lib/letsencrypt" \
  -v "${webroot_mount}" \
  certbot/certbot renew --webroot -w "${WEBROOT}" \
  --non-interactive --deploy-hook "touch ${FLAG}" ${extra}

if [ "${DRY_RUN}" = "1" ]; then
    echo "$(date '+%F %T') Essai a blanc termine, aucun redemarrage."
    exit 0
fi

if docker run --rm -v "${VOL_ETC}:/etc/letsencrypt" alpine test -f "${FLAG}"; then
  echo "$(date '+%F %T') Certificat renouvele -> redemarrage ${SERVER_NAME}"
  docker restart "${SERVER_NAME}"
  docker run --rm -v "${VOL_ETC}:/etc/letsencrypt" alpine rm -f "${FLAG}"
else
  echo "$(date '+%F %T') Aucun renouvellement necessaire"
fi
