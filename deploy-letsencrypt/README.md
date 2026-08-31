# Certificat Let's Encrypt pour le serveur NVDA Remote

Cette procédure fait servir au serveur NVDA Remote un **certificat TLS signé par
Let's Encrypt** pour un ou deux domaines, au lieu du certificat auto-signé par
défaut. Certains proxies exigent un **nom de domaine** avec un certificat validé
par une autorité reconnue ; se connecter à un nom de domaine (et non à une
adresse IP) résout ce problème.

## Configuration

Tous les paramètres se trouvent dans un fichier `.env` local, **jamais
versionné**. Partez du modèle fourni :

```sh
cp .env.example .env
$EDITOR .env
```

Les deux scripts lisent ce fichier automatiquement. Si `.env` est absent, des
valeurs d'exemple neutres sont utilisées, ce qui empêche un déploiement
accidentel sur un domaine qui ne vous appartient pas.

Aucun secret ne transite par ce fichier. Les secrets du serveur restent dans des
fichiers montés dans le conteneur, avec des droits restreints :
`-turn-secret-file` pour le secret TURN, `-admin-password-file` pour le
condensat du mot de passe d'administration, et le volume certbot en lecture
seule pour la clé privée TLS.

## État type d'un déploiement

- Le conteneur du serveur écoute en TLS signé sur **6837** (protocole
  historique) et sur **443** (WebSocket sécurisé, `-ws-address :443`, qui accepte
  également le protocole historique grâce au multiplexage, voir
  [TCP-and-WebSocket.md](../TCP-and-WebSocket.md)).
- Certificat Let's Encrypt stocké dans un volume Docker, monté en lecture seule
  dans le conteneur.
- Validité **90 jours**, renouvellement automatique via cron utilisateur.

## Comment ça marche

Le port 80 de l'hôte est généralement déjà occupé par un serveur web. On ne peut
donc pas y lancer un nginx dédié. La validation Let's Encrypt (challenge
**HTTP-01**) passe par le webroot existant :

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
| `.env.example` | Modèle de configuration. À copier en `.env`, qui n'est pas versionné. |
| `deploy-letsencrypt.sh` | Déploiement complet (webroot + émission + conteneur + cron). |
| `renew.sh` | Renouvellement + redémarrage conditionnel, appelé par cron. |

## Réexécuter / adapter

Ajustez votre fichier `.env` (`DOMAIN`, `ADDITIONAL_DOMAIN`, `EMAIL`, `WEBROOT`,
`SERVER_ARGS`, `SERVER_PORTS`) puis :

```sh
./deploy-letsencrypt.sh
```

Test à blanc sans consommer le quota Let's Encrypt : mettez `STAGING=1` dans
`.env`, lancez le script, vérifiez, puis repassez à `STAGING=0`.

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
. ./.env

# Certificat servi sur 6837 (protocole historique)
echo | openssl s_client -connect "${DOMAIN}:6837" 2>/dev/null \
  | openssl x509 -noout -issuer -subject -dates

# Certificat servi sur 443 (WebSocket securise)
echo | openssl s_client -connect "${DOMAIN}:443" -servername "${DOMAIN}" 2>/dev/null \
  | openssl x509 -noout -issuer -dates
```

Résultat attendu : `issuer = Let's Encrypt`, avec les deux noms présents dans
`subjectAltName`.

## Côté client (add-on NVDA)

Utilisez le **nom de domaine** (et non l'adresse IP), port `6837` ou `443`.
Depuis le multiplexage, les deux ports acceptent aussi bien le protocole
historique que le WebSocket sécurisé : les anciens clients continuent donc de
fonctionner sur `443`. Voir [TCP-and-WebSocket.md](../TCP-and-WebSocket.md).
