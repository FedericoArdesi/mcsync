# mcsync

*A shared Minecraft Java world for a small group of friends, without a 24/7 server: whoever plays hosts, an always-on laptop keeps the world, the versions and the "who is host" lease. Alpha — see [Status](#stato).*

Un mondo Minecraft Java condiviso tra amici, senza un server sempre acceso. Il server di gioco gira sul PC di chi gioca; un laptop (o un mini-PC) sempre acceso tiene il mondo, le versioni e la **lease**, cioè "chi è l'host adesso". Nessun costo di hosting, nessuna perdita di dati, trasferimenti incrementali.

> mcsync non è affiliato a Mojang, Microsoft o Minecraft. "Minecraft" è un marchio dei rispettivi proprietari. Il programma non contiene né distribuisce nulla di Minecraft: serve il tuo `server.jar`.

## Stato

**Alpha.** Il nucleo è completo e ha una suite di test estesa (protocollo, servizio, agente, passaggio di host, comandi), ma **è stato provato solo con un gioco simulato**, non ancora su un mondo Minecraft vero con mod e giocatori collegati. Non usarlo come unica copia di un mondo a cui tieni.

Cosa non c'è ancora: interfaccia web, bot Telegram, sito di stato, trasferimento automatico dei giocatori al nuovo host durante un passaggio. Vedi [Limiti noti](#limiti-noti).

## Come funziona

Ci sono due programmi:

- **`mcsyncd`** gira sul laptop sempre acceso. Offre una API HTTP, tiene l'archivio delle versioni del mondo (con [restic](https://restic.net)), emette inviti e gestisce i dispositivi.
- **`mcsync`** gira sul PC di chi vuole giocare. Scarica l'ultima versione, avvia il server Minecraft, lo comanda dallo stdin (niente RCON), ogni 10 minuti salva e carica sul laptop, e alla chiusura carica la versione finale e rilascia la lease.

Regole principali:

- **Un solo host alla volta.** La lease dura 10 minuti, si rinnova con un heartbeat ogni minuto, e conta solo l'orologio del laptop. Ogni nuova lease incrementa una generazione.
- **Una versione diventa ufficiale solo se** i blocchi sono integri, la generazione è quella giusta, la base coincide con il puntatore ufficiale (compare-and-swap) e lo spazio basta. Altrimenti l'upload finisce tra gli orfani, senza perdersi.
- **Passaggio di host.** Se un altro gioca, `mcsync handoff` gli chiede di salvare e passarti la mano: lui ferma il gioco, carica e rilascia, la lease resta riservata a te per 3 minuti, poi parti dalla sua versione.
- **Se il laptop non risponde**, l'host si ferma da solo prima che la lease scada (avviso a 3 minuti, stop a 8).
- **Rete privata.** Pensato per girare su [Tailscale](https://tailscale.com) (o una rete simile): nessuna porta aperta sul router. L'accesso usa token per dispositivo, revocabili.

## Requisiti

- **Laptop (`mcsyncd`)**: Linux (sviluppato su Ubuntu), [restic](https://restic.net) nel `PATH`, un indirizzo di rete privata (per esempio quello di Tailscale).
- **PC dei giocatori (`mcsync`)**: Windows o Linux, e per giocare davvero Java e un `server.jar` di Minecraft. Senza `--server-jar` parte un gioco finto, utile per provare tutto il giro.
- **Go 1.22 o successivo** per compilare. Nessuna dipendenza esterna.

## Installazione

```sh
go install github.com/FedericoArdesi/mcsync/cmd/mcsyncd@latest   # sul laptop
go install github.com/FedericoArdesi/mcsync/cmd/mcsync@latest    # sui PC dei giocatori
```

Oppure, dalla cartella del progetto: `go build ./cmd/mcsync ./cmd/mcsyncd`.

## Avvio rapido

**Sul laptop**

```sh
mcsyncd init   --data /srv/mcsync
mcsyncd serve  --data /srv/mcsync --listen 100.x.y.z:8765
mcsyncd invite --data /srv/mcsync --addr 100.x.y.z:8765 Fede   # un invito per giocatore
```

`100.x.y.z` è l'indirizzo del laptop sulla rete privata. Il nome del giocatore ha 3-16 caratteri tra lettere, cifre e trattino basso. Altri comandi: `mcsyncd devices` elenca i dispositivi, `mcsyncd revoke ID` ne revoca uno.

**Sul PC di un giocatore**

```sh
mcsync enroll CODICE --label "PC di Fede"        # una volta sola, con il codice dell'invito
mcsync status                                     # chi è host, versione ufficiale
mcsync run --world C:\mondi\survival \
           --server-jar C:\server\server.jar \
           --java "C:\Program Files\Java\bin\java.exe" \
           --jvm-args "-Xmx4G"                    # avvia la sessione; Ctrl+C salva e rilascia
mcsync handoff --world C:\mondi\survival ...      # come run, ma chiede prima il passaggio a chi sta giocando
```

Con `mcsync <comando> -h` si vedono tutte le opzioni (ogni quanto salvare, ogni quanto rinnovare la lease, timeout del passaggio e altre).

## Limiti noti

- Provato solo con il gioco finto: tempi di stop e salvataggio finale, e cosa succede ai giocatori collegati durante un passaggio di host, con Minecraft vero sono da verificare.
- Un passaggio di host non sposta da solo i giocatori sul nuovo server: devono riconnettersi.
- Non verificati: layout del mondo per versioni vecchie e per Paper, file `*_old`, comportamento a server in pausa, Java per loader vecchi.
- Test di rete reale (relay Tailscale, banda tra case diverse) non fatto.
- Con un motore di archivio esterno resta da definire come impedire ai client di cancellare versioni.
- I commenti nel codice e i messaggi sono in italiano, e alcuni rimandano a sezioni di un documento di progetto non incluso in questa repository.

## Sviluppo

```sh
go vet ./...
go test ./...
```

La CI (GitHub Actions) esegue gli stessi comandi su Linux e Windows a ogni push.

## Licenza

[MIT](LICENSE).
