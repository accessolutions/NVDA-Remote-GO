# Certificat Let's Encrypt pour nvdaremote.accessolutions.fr (serveur NVDA Remote)

Cette procédure fait servir au serveur NVDA Remote un **certificat TLS signé par
Let's Encrypt** pour le domaine `nvdaremote.accessolutions.fr`, au lieu du certificat
auto-signé par défaut. Certains proxies exigent un **nom de domaine** avec un
certificat validé par une autorité reconnue ; se connecter à
`nvdaremote.accessolutions.fr` (et non à une adresse IP) résout ce problème.

## État déployé (serveur `sd-david`, 31/07/2026)

- Domaine : `nvdaremote.accessolutions.fr` (DNS OK).
- Le conteneur `nvdaremote` (image `nvdaremoteserver-docker`) écoute en TLS
  signé sur **6837** (TLS brut) et **443** (WebSocket sécurisé, `-ws-address :443`).
- Certificat Let's Encrypt stocké dans le volume Docker `le-etc`
  (`/etc/letsencrypt/live/nvdaremote.accessolutions.fr/`), monté en lecture seule dans
  le conteneur.
- Validité **90 jours**, renouvellement automatique via cron utilisateur.

## Comment ça marche

Le port 80 de l'hôte est déjà occupé par **Apache** (DocumentRoot
`/var/www/html` pour le domaine). On ne peut donc pas y lancer un nginx dédié.
La validation Let's Encrypt (challenge **HTTP-01**) passe par ce webroot Apache :

1. On crée `/var/www/html/.well-known/acme-challenge/` avec un `.htaccess`
   (`Require all granted`) pour lever l'authentification **uniquement** sur ce
   chemin. L'écriture se fait via un conteneur Docker jetable (le démon Docker
   tourne en root), **sans avoir besoin de `sudo`**.
2. `certbot` (conteneur jetable) obtient le certificat en mode `--webroot`.
3. Le conteneur `nvdaremote` est recréé avec le volume `le-etc` monté et
   `-cert-file fullchain.pem -key-file privkey.pem`.
4. Une tâche cron renouvelle et redémarre le conteneur si le certificat change.

> Le port 80 n'a **aucun impact** sur l'accès distant NVDA Remote. Il sert
> uniquement à la validation du domaine par l'autorité de certification.

## Fichiers

| Fichier | Rôle |
|---|---|
| `deploy-letsencrypt.sh` | Déploiement complet (webroot + émission + conteneur + cron). |
| `renew.sh` | Renouvellement + redémarrage conditionnel. Copié en `~/nvdaremote-renew.sh` et appelé par cron. |

## Réexécuter / adapter

Ajustez les variables en tête de `deploy-letsencrypt.sh` (`DOMAIN`, `EMAIL`,
`WEBROOT`, `SERVER_ARGS`, `SERVER_PORTS`) puis :

```sh
./deploy-letsencrypt.sh
```

Test à blanc sans consommer le quota Let's Encrypt : mettez `STAGING=1` en tête
du script, lancez-le, vérifiez, puis repassez à `STAGING=0`.

## Renouvellement automatique

- Cron utilisateur (déjà installé) : `17 3,15 * * *` → `~/nvdaremote-renew.sh`.
- certbot ne renouvelle qu'à **~30 jours** de l'expiration ; en cas de
  renouvellement, le conteneur `nvdaremote` est redémarré.
- Journal : `~/nvdaremote-renew.log`.

Renouvellement / vérification manuelle :

```sh
~/nvdaremote-renew.sh
```

## Vérifications

```sh
# Certificat servi sur 6837 (TLS brut)
echo | openssl s_client -connect nvdaremote.accessolutions.fr:6837 2>/dev/null \
  | openssl x509 -noout -issuer -subject -dates

# Certificat servi sur 443 (WebSocket sécurisé)
echo | openssl s_client -connect nvdaremote.accessolutions.fr:443 -servername nvdaremote.accessolutions.fr 2>/dev/null \
  | openssl x509 -noout -issuer -dates
```

Résultat attendu : `issuer = Let's Encrypt`, `subject = nvdaremote.accessolutions.fr`.

## Côté client (add-on NVDA)

Utilisez l'hôte **`nvdaremote.accessolutions.fr`** (et non l'adresse IP), port `6837`
(ou `443` pour le transport WebSocket sécurisé selon la configuration du client).
