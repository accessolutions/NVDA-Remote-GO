# Signalisation WebRTC — spécification d'implémentation côté client

Ce document décrit précisément ce qu'il faut écrire côté client pour utiliser la
signalisation ajoutée au serveur NVDA Remote. Il complète
[docs/partage-ecran-webrtc.md](docs/partage-ecran-webrtc.md), qui expose les choix
d'architecture ; ici, seule l'implémentation concrète est traitée.

Le moteur vidéo est un **navigateur Chromium** installé sur le poste — Microsoft
Edge de préférence, Google Chrome ou Brave à défaut — piloté par TeleNVDA à
travers une page servie sur `127.0.0.1`. Il n'y a **aucun exécutable à écrire, à
signer ou à distribuer** : capture, encodage et transport WebRTC sont fournis par
le navigateur, TeleNVDA ne conserve que la signalisation et l'injection des
entrées par `ctypes`.

**Périmètre retenu : session utilisateur uniquement.** Tout s'exécute dans la
session interactive de l'utilisateur, avec ses droits. Le bureau sécurisé
(Winlogon, UAC, écran de verrouillage) est hors périmètre : la capture s'y
interrompt et l'injection d'entrées y est refusée par Windows. La section 9
décrit le comportement attendu dans ce cas.

---

## 1. Vue d'ensemble

Par poste :

| Composant | Rôle | Technique |
|---|---|---|
| NVDA + TeleNVDA | connexion existante au serveur, interface utilisateur, envoi et réception des messages de signalisation, injection des entrées | Python |
| Navigateur Chromium | capture d'écran, encodage, transport WebRTC, affichage | page locale servie sur `127.0.0.1` |
| Serveur NVDA Remote | routage de la signalisation, distribution des identifiants TURN | Go, déjà déployé |

Le média **ne passe jamais** par le serveur NVDA Remote. Il transite en pair à
pair, ou par un relais TURN si le pair à pair échoue.

```mermaid
sequenceDiagram
    participant MN as Navigateur maître
    participant M as TeleNVDA maître
    participant S as Serveur
    participant E as TeleNVDA esclave
    participant EN as Navigateur esclave
    M->>S: capabilities
    E->>S: capabilities
    M->>S: turn_credentials
    S-->>M: ice_servers
    M->>S: screen_share_request (target)
    S->>E: screen_share_request (origin)
    E-->>E: consentement utilisateur
    E->>S: screen_share_response accepted
    S-->>M: screen_share_response
    E->>EN: démarrage capture
    EN-->>E: SDP offre
    E->>S: webrtc_offer (target)
    S->>M: webrtc_offer (origin)
    M->>MN: SDP offre
    MN-->>M: SDP réponse
    M->>S: webrtc_answer (target)
    S->>E: webrtc_answer (origin)
    MN-->>EN: flux vidéo + DataChannel souris
```

Point crucial : **l'offre SDP part de l'esclave**, c'est-à-dire du poste dont
l'écran est partagé, puisque c'est lui qui possède la piste vidéo.

---

## 2. Serveur de test disponible

L'instance de test est déployée et fonctionnelle, **en parallèle de la production
qui reste inchangée**.

| Paramètre | Valeur |
|---|---|
| Hôte | `nvdaremote.accessolutions.fr` |
| WebSocket | `wss://nvdaremote.accessolutions.fr:8443/` |
| TLS brut (protocole historique) | `nvdaremote.accessolutions.fr:6838` |
| Sous-protocole WebSocket | `nvdaremote/2.0` |
| Certificat | Let's Encrypt, valide jusqu'au 5 novembre 2026 |
| Partage d'écran | activé (`-screen-share`) |
| Conteneur | `nvdaremote-screenshare-test` |

La production reste sur `:443` (WebSocket) et `:6837` (TLS brut) et n'a pas la
fonctionnalité activée.

**Avertissement TURN.** Les URL `stun:nvdaremote.accessolutions.fr:3478` et
`turn:nvdaremote.accessolutions.fr:3478?transport=udp` sont configurées, mais
**aucun serveur coturn n'est encore installé**. Le serveur renverra donc ces URL,
mais elles ne répondront pas. Pour les premiers essais, utilisez un STUN public
(par exemple `stun:stun.l.google.com:19302`) ; le pair à pair fonctionnera dans
la plupart des cas hors NAT symétrique.

---

## 3. Protocole de signalisation — messages exacts

Tous les messages sont du JSON encodé en UTF-8, **une ligne par message,
terminée par `\n`**, comme le protocole NVDA Remote existant.

### 3.1 `capabilities` — déclaration des capacités

Envoyé par le client, dans les deux sens, **avant ou après le `join`**. À
renvoyer après chaque reconnexion.

```json
{"type": "capabilities", "capabilities": ["screen_share/1", "input_control/1"]}
```

Le serveur applique une liste blanche : seules `screen_share/1` et
`input_control/1` sont acceptées, les valeurs inconnues sont silencieusement
supprimées, les doublons éliminés, et la liste plafonnée à 8 entrées. Le serveur
n'envoie aucun accusé de réception.

Les capacités sont ensuite diffusées aux autres clients du canal dans
`client_joined` et `channel_joined` :

```json
{"type": "client_joined", "client": {"id": 3, "connection_type": "slave", "capabilities": ["screen_share/1", "input_control/1"]}}
```

```json
{"type": "channel_joined", "origin": 4, "clients": [{"id": 3, "connection_type": "slave", "capabilities": ["screen_share/1"]}], "channel": "monCanal", "user_ids": [3]}
```

Le champ `capabilities` est **absent** si le client n'a rien déclaré. TeleNVDA
doit donc traiter son absence comme « ne sait pas partager l'écran » et griser
l'option correspondante.

### 3.2 `turn_credentials` — demande d'identifiants ICE

Requête, sans paramètre :

```json
{"type": "turn_credentials"}
```

Réponse :

```json
{"type": "turn_credentials", "ice_servers": [{"urls": ["stun:nvdaremote.example.com:3478"]}, {"urls": ["turn:nvdaremote.example.com:3478?transport=udp"], "username": "1786425600:7", "credential": "EXEMPLE_HMAC_BASE64"}], "ttl": 3600}
```

- `username` a la forme `<expiration_unix>:<id_utilisateur>`.
- `credential` est `base64(HMAC-SHA1(secret_partagé, username))`. La valeur
  ci-dessus est un remplissage : un identifiant réel ne doit jamais être
  recopié dans un document versionné.
- `ttl` est la durée de validité en secondes.

Ces identifiants doivent être demandés **juste avant** de créer la
`RTCPeerConnection`, jamais mis en cache au-delà de `ttl` moins une marge de
60 secondes. Pour une session longue, redemandez-les avant expiration ; ils ne
sont utilisés qu'à l'allocation TURN, une session déjà établie n'est pas
interrompue par leur expiration.

Erreurs possibles : `screen_share_unsupported` (capacité non déclarée),
`not_authorized` (maître non autorisé), `turn_unavailable` (aucune URL
configurée), `invalid_parameters` (pas encore dans un canal).

### 3.3 `screen_share_request` — demande de partage

Envoyé par le **maître** vers l'**esclave** dont il veut voir l'écran.

```json
{"type": "screen_share_request", "target": 3, "quality": "auto", "cursor": true, "input": true}
```

| Champ | Type | Obligatoire | Sens |
|---|---|---|---|
| `target` | entier | oui | identifiant du destinataire |
| `quality` | chaîne | non | `"low"`, `"medium"`, `"high"` ou `"auto"` |
| `cursor` | booléen | non | inclure le pointeur dans l'image |
| `input` | booléen | non | le maître demande aussi le contrôle des entrées |

Le serveur ajoute `origin` et transmet **au seul destinataire** :

```json
{"type": "screen_share_request", "target": 3, "quality": "auto", "cursor": true, "input": true, "origin": 4}
```

Les champs inconnus du serveur sont transmis tels quels : le protocole applicatif
entre les deux clients reste extensible sans modifier le serveur.

### 3.4 `screen_share_response` — acceptation ou refus

Envoyé par l'esclave, après consentement explicite de son utilisateur.

Acceptation :

```json
{"type": "screen_share_response", "target": 4, "accepted": true, "input": true, "screens": [{"index": 0, "width": 1920, "height": 1080, "primary": true}, {"index": 1, "width": 2560, "height": 1440, "primary": false}]}
```

Refus :

```json
{"type": "screen_share_response", "target": 4, "accepted": false, "reason": "declined"}
```

Valeurs de `reason` recommandées : `"declined"` (refus utilisateur),
`"unavailable"` (aucun navigateur pris en charge, ou moteur en échec), `"busy"`
(déjà en session), `"timeout"` (aucune réponse dans le délai imparti).

`input` dans la réponse est la décision finale de l'esclave : le maître peut
demander le contrôle, l'esclave peut n'accorder que la vue.

### 3.5 `webrtc_offer` — offre SDP

Émis par l'esclave.

```json
{"type": "webrtc_offer", "target": 4, "sdp": "v=0\r\no=- 4611731400430051336 2 IN IP4 127.0.0.1\r\n..."}
```

Le SDP est transmis **tel quel, avec ses `\r\n` échappés en JSON**. Ne le
reformatez ni ne le tronquez : un SDP complet dépasse souvent 4 Ko.

### 3.6 `webrtc_answer` — réponse SDP

Émis par le maître.

```json
{"type": "webrtc_answer", "target": 3, "sdp": "v=0\r\no=- 8123... \r\n..."}
```

### 3.7 `webrtc_candidate` — candidat ICE

Émis dans les deux sens, à chaque candidat découvert (trickle ICE).

```json
{"type": "webrtc_candidate", "target": 3, "candidate": "candidate:842163049 1 udp 1677729535 90.12.34.56 51234 typ srflx raddr 192.168.1.20 rport 51234 generation 0 ufrag k3Rf network-cost 999", "sdpMid": "0", "sdpMLineIndex": 0}
```

Fin de collecte : envoyez `"candidate": ""`.

### 3.8 `screen_share_stop` — arrêt

Émis par l'une ou l'autre extrémité.

```json
{"type": "screen_share_stop", "target": 3, "reason": "user_stopped"}
```

Valeurs de `reason` : `"user_stopped"`, `"error"`, `"disconnected"`,
`"secure_desktop"`.

À l'arrêt, fermez la `RTCPeerConnection`, libérez les ressources de capture, et
émettez un signal sonore côté esclave.

### 3.9 Erreurs renvoyées par le serveur

```json
{"type": "error", "error": "target_not_found"}
```

| Code | Cause |
|---|---|
| `screen_share_unsupported` | l'émetteur ou le destinataire n'a pas déclaré `screen_share/1` |
| `not_authorized` | maître non authentifié sur un serveur avec mot de passe |
| `invalid_parameters` | `target` absent, nul, négatif, ou égal à son propre identifiant |
| `target_not_found` | destinataire inexistant dans le canal, ou maître non autorisé |
| `turn_unavailable` | aucune URL ICE configurée sur le serveur |

**Limite de débit.** Le serveur accepte au maximum 250 messages de signalisation
par tranche de 10 secondes et par client. Au-delà, les messages sont
**silencieusement rejetés**, sans message d'erreur. Cette marge est largement
suffisante pour du trickle ICE ; elle ne l'est pas pour transporter des données
applicatives. N'utilisez jamais la signalisation comme canal de données.

---

## 4. Séquence complète, pas à pas

Côté maître (celui qui regarde) :

1. À la connexion : envoyer `capabilities`.
2. Sur réception de `channel_joined` ou `client_joined`, mémoriser quels
   identifiants possèdent `screen_share/1`. Activer l'option « Afficher l'écran
   distant » uniquement pour ceux-là.
3. À l'activation par l'utilisateur : envoyer `turn_credentials`, attendre la
   réponse.
4. Envoyer `screen_share_request` avec `target`.
5. Armer un délai d'attente de 30 secondes. Sans `screen_share_response`,
   abandonner et informer l'utilisateur.
6. Sur `screen_share_response` avec `accepted: true`, ouvrir la page locale en
   mode récepteur en lui transmettant les `ice_servers`.
7. Sur `webrtc_offer`, transmettre le SDP à la page, récupérer la réponse,
   l'envoyer en `webrtc_answer`.
8. Relayer les `webrtc_candidate` dans les deux sens.
9. Sur connexion établie, afficher la fenêtre vidéo.

Côté esclave (celui dont l'écran est partagé) :

1. À la connexion : envoyer `capabilities`.
2. Sur `screen_share_request`, **afficher une demande de consentement explicite**
   indiquant l'identifiant du demandeur et si le contrôle des entrées est
   demandé. Ne jamais accepter automatiquement.
3. Après acceptation : envoyer `turn_credentials`, ouvrir la page locale en mode
   émetteur avec les `ice_servers` et le drapeau `input`.
4. Envoyer `screen_share_response` avec `accepted: true` et la liste des écrans.
5. La page produit l'offre : l'envoyer en `webrtc_offer`.
6. Sur `webrtc_answer`, la transmettre à la page.
7. Relayer les `webrtc_candidate`.
8. Émettre un signal sonore distinctif au démarrage et à l'arrêt du partage, et
   afficher un indicateur persistant.

---

## 5. Canal de données pour les entrées

Un `RTCDataChannel` nommé `input`, créé par l'esclave avec :

```
ordered: true
maxRetransmits: 0
```

C'est-à-dire un canal **ordonné mais non fiable** : perdre un mouvement de souris
est sans conséquence, en revanche l'ordre des clics compte.

### 5.1 Format des messages

JSON compact, champs volontairement courts pour limiter le débit. Coordonnées
**normalisées dans `[0, 1]`** par rapport à l'écran partagé, ce qui rend le
protocole indépendant des résolutions des deux postes.

Mouvement :

```json
{"t":"m","x":0.4312,"y":0.7788}
```

Bouton enfoncé et relâché (`b` : 0 gauche, 1 milieu, 2 droit) :

```json
{"t":"md","x":0.4312,"y":0.7788,"b":0}
{"t":"mu","x":0.4312,"y":0.7788,"b":0}
```

Molette (`d` : cran vertical, positif vers le haut ; `h` : horizontal) :

```json
{"t":"w","x":0.4312,"y":0.7788,"d":-3}
```

Touche enfoncée et relâchée (`c` : code de touche virtuelle Windows, `m` :
masque de modificateurs, bit 0 Maj, bit 1 Ctrl, bit 2 Alt, bit 3 Windows) :

```json
{"t":"kd","c":65,"m":2}
{"t":"ku","c":65,"m":2}
```

Le maître limite les mouvements à 60 messages par seconde et **n'envoie pas** de
mouvement si le déplacement est inférieur à un pixel de l'écran distant.

### 5.2 Injection sous Windows

Utiliser `SendInput` avec `MOUSEEVENTF_ABSOLUTE | MOUSEEVENTF_VIRTUALDESK`. La
conversion est la partie la plus souvent ratée :

1. Partir des coordonnées normalisées `(x, y)` de l'écran partagé.
2. Les convertir en pixels de cet écran : `px = x * largeur_ecran`,
   `py = y * hauteur_ecran`.
3. Les décaler vers le bureau virtuel en ajoutant l'origine de l'écran, obtenue
   par `GetMonitorInfo`. L'origine peut être **négative** si l'écran est à gauche
   ou au-dessus de l'écran principal.
4. Normaliser sur le bureau virtuel, dont les dimensions sont données par
   `SM_XVIRTUALSCREEN`, `SM_YVIRTUALSCREEN`, `SM_CXVIRTUALSCREEN`,
   `SM_CYVIRTUALSCREEN` :

$$
dx = \frac{(px_{virtuel} - X_{virtuel}) \times 65535}{CX_{virtuel} - 1}
$$

$$
dy = \frac{(py_{virtuel} - Y_{virtuel}) \times 65535}{CY_{virtuel} - 1}
$$

5. Renseigner `MOUSEINPUT.dx = dx`, `MOUSEINPUT.dy = dy`.

Le processus qui injecte doit être **déclaré `PerMonitorV2`**, sinon Windows lui
renvoie des coordonnées virtualisées et le pointeur sera décalé sur les
configurations à échelle mixte. NVDA l'est déjà.

Pour le clavier, utiliser `KEYEVENTF_SCANCODE` avec les codes matériels obtenus
par `MapVirtualKey`, et `KEYEVENTF_EXTENDEDKEY` pour les touches étendues
(flèches, Inser, Suppr, Origine, Fin, Page précédente, Page suivante, pavé
numérique Entrée, Ctrl et Alt de droite). Omettre ce drapeau est la cause
classique des flèches qui produisent des chiffres du pavé numérique.

### 5.3 Refus par Windows

`SendInput` échoue silencieusement, en renvoyant 0, dans deux cas fréquents :

- le bureau sécurisé est actif ;
- l'application ciblée s'exécute avec des privilèges plus élevés que NVDA
  (UIPI). Un NVDA lancé en tant qu'utilisateur standard ne peut pas piloter une
  fenêtre élevée.

Le client doit vérifier la valeur de retour et remonter une erreur explicite
plutôt que de laisser l'utilisateur croire que le contrôle fonctionne.

---

## 6. Réglages de l'image partagée

Le contenu à transmettre est du texte, pas de la vidéo : la netteté prime sur la
fluidité. Un texte net à 5 images par seconde est bien plus utile qu'un texte
flou à 30.

Sur la piste vidéo obtenue par `getDisplayMedia()`, positionner
`contentHint = "detail"`, qui demande au navigateur de privilégier la netteté
spatiale, puis plafonner le débit sur l'encodage du `RTCRtpSender` avec
`maxBitrate`, `maxFramerate` et
`degradationPreference = "maintain-resolution"`.

Profils proposés à l'utilisateur :

| Profil | Largeur maximale | Débit cible |
|---|---|---|
| `low` | 1280 px | 400 kb/s |
| `balanced` | 1600 px | 1200 kb/s |
| `high` | 1920 px | 3500 kb/s |

Le nombre d'images par seconde est réglable séparément, par défaut 15. Ne jamais
dépasser 1920 pixels de large sans raison : au-delà, le coût d'encodage devient
perceptible sur le poste assisté, ce qui est inacceptable pour un utilisateur de
lecteur d'écran.

Côté codec, laisser le navigateur négocier : il propose VP8 et VP9, tous deux
libres de redevances, VP9 apportant environ 30 % de débit en moins sur du contenu
d'écran.

---

## 7. Sécurité — exigences non négociables

| Exigence | Mise en œuvre |
|---|---|
| Consentement explicite | boîte de dialogue modale côté esclave, jamais de mémorisation « toujours accepter » silencieuse |
| Indication permanente | signal sonore au démarrage et à l'arrêt, plus un indicateur permanent accessible au lecteur d'écran |
| Arrêt d'urgence | raccourci clavier global côté esclave coupant instantanément flux et contrôle, sans confirmation |
| Isolation de la page locale | serveur HTTP borné à `127.0.0.1`, jeton aléatoire à usage unique, refus de toute autre source |
| Pas de secret en ligne de commande | jeton et identifiants TURN transmis hors `argv`, ceux-ci étant lisibles par les autres processus du poste |
| Profil de navigateur jetable | répertoire temporaire dédié, jamais le profil de l'utilisateur, supprimé en fin de session |
| Chiffrement | DTLS-SRTP obligatoire, imposé par WebRTC ; refuser toute session non chiffrée |
| Vérification de l'origine | ne traiter un `webrtc_offer` que si `origin` correspond au pair accepté dans `screen_share_response` |
| Portée du contrôle | si `input` vaut `false`, l'esclave ne doit **pas ouvrir** le canal `input`, et non se contenter d'ignorer les messages |

Le dernier point est essentiel : la sécurité doit reposer sur l'absence du canal,
pas sur un filtrage applicatif qu'un pair modifié pourrait contourner.

---

## 8. Reconnexion et robustesse

- Si la connexion WebSocket au serveur tombe, la session WebRTC déjà établie
  **survit** : le média est en pair à pair. Le partage doit continuer. En
  revanche, toute nouvelle négociation devient impossible ; afficher un
  avertissement.
- Si la `RTCPeerConnection` passe en `failed`, tenter un redémarrage ICE une
  seule fois, puis abandonner et émettre `screen_share_stop` avec
  `reason: "error"`.
- Si le navigateur est fermé ou tué, TeleNVDA doit le détecter par la fermeture
  du pont local, émettre `screen_share_stop` avec `reason: "error"`, et proposer
  un redémarrage manuel — jamais une boucle de relance automatique.
- Après reconnexion au serveur, renvoyer `capabilities` avant toute autre action.

---

## 9. Bureau sécurisé

Hors périmètre pour cette implémentation, mais le comportement doit être défini :

1. Détecter le basculement de bureau, par exemple avec `OpenInputDesktop` : la
   capture du navigateur se fige et l'injection d'entrées est refusée.
2. En informer le maître par un message applicatif sur le canal existant.
3. Le maître affiche « bureau sécurisé, image suspendue » au lieu d'une image
   figée trompeuse.
4. Au retour dans la session utilisateur, l'émission reprend sans renégociation
   SDP.

Ne jamais laisser la dernière image affichée : l'assistant croirait voir l'état
réel du poste.

---

## 10. Rappel sur coturn

Aucun serveur TURN n'est déployé. Quand ce sera le cas, la configuration doit
comporter :

```
use-auth-secret
static-auth-secret=<le même secret que celui du serveur NVDA Remote>
realm=nvdaremote.accessolutions.fr
listening-port=3478
tls-listening-port=5349
```

Le secret doit être **identique** à celui du fichier désigné par
`-turn-secret-file`, sans quoi les identifiants générés seront systématiquement
rejetés. Les ports 3478 et 5349 sont libres sur le serveur.
