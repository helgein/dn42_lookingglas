# dn42-lg

Live-Looking-Glass für ein kleines dn42-AS. Ein statisches Go-Binary ohne Abhängigkeiten, Web-UI eingebettet.

- spricht direkt mit dem BIRD-Control-Socket (kein birdc, kein Agent-Zoo)
- schaltet jede Socket-Verbindung per `restrict` in den Nur-Lesen-Modus
- Topologie mit eigenem AS in der Mitte, Updates/s als Linienstärke und Fluss
- Session-Tabelle mit Änderungs-Highlight wie `watch -d`, 90-s-Verlauf
- Matrix: welches eigene Präfix wird an welchen Peer exportiert
- Abfragen: `show route for <IP|Präfix> all` und Routen über ein AS
- mehrere Router: ein Knoten aggregiert die anderen über `-remote`

## Bauen

Go ≥ 1.22, nur Standardbibliothek. Am einfachsten in WSL bauen:

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o dn42-lg .
```

Für Hetzner-ARM-Instanzen (CAX) `GOARCH=arm64`.

## Installieren (beide Hosts)

```bash
install -m 0755 dn42-lg /usr/local/bin/dn42-lg
install -m 0644 dn42-lg.service /etc/systemd/system/dn42-lg.service
install -m 0644 dn42-lg.default.<host> /etc/default/dn42-lg
ls -l /run/bird/bird.ctl          # Gruppe muss "bird" sein, Modus 0660
systemctl daemon-reload && systemctl enable --now dn42-lg
journalctl -u dn42-lg -f
```

runkel-eval liefert nur seine Daten, spare-1 aggregiert beide.
Aufruf im Browser: `http://172.20.15.129:8042/`

## Firewall (Salt-Pillar)

spare-1, Zugriff von den Notebooks:

```yaml
  open_rw_lg_4:
    chain: input_open
    family: ipv4
    jump: ACCEPT
    dports: "8042"
    in-interface: "wg-rw"
    proto: tcp
```

runkel-eval, Abfrage durch spare-1 über den iBGP-Tunnel:

```yaml
  open_int_lg_4:
    chain: input_open
    family: ipv4
    jump: ACCEPT
    dports: "8042"
    in-interface: "dn42-int"
    proto: tcp
```

Für ganz dn42 sichtbar, auf spare-1 zusätzlich (IPv4 und IPv6):

```yaml
  open_dn42_lg_4:
    chain: input_open
    family: ipv4
    jump: ACCEPT
    dports: "8042"
    in-interface: "dn42+"
    proto: tcp

  open_dn42_lg_6:
    chain: input_open
    family: ipv6
    jump: ACCEPT
    dports: "8042"
    in-interface: "dn42+"
    proto: tcp
```

Nicht auf `[::]` oder `0.0.0.0` binden, sonst hängt das LG auch an eth0.

## Optionen

| Flag | Default | Bedeutung |
|---|---|---|
| `-listen` | `127.0.0.1:8042` | Listen-Adressen, kommagetrennt, IPv6 in eckigen Klammern |
| `-socket` | `/run/bird/bird.ctl` | BIRD-Control-Socket |
| `-name` | Hostname | Name dieses Routers |
| `-prefixes` | leer | eigene Präfixe für die Ankündigungs-Matrix |
| `-remote` | – | `name=http://ip:port`, mehrfach möglich |
| `-interval` | `1s` | Abfrageintervall (min. 500 ms) |
| `-asn` | aus BIRD | eigene ASN |
| `-title` | Routernamen | Untertitel |

## Sicherheit

- jede Socket-Verbindung beginnt mit `restrict`, BIRD lehnt danach alles außer `show …` ab
- Abfragen nehmen nur geparste IPs/Präfixe bzw. numerische ASNs an, nie freien Text
- höchstens zwei Abfragen gleichzeitig, Ausgabe auf 1000 Zeilen begrenzt
- nur GET, CSP `default-src 'self'`, keine externen Ressourcen
- systemd: `DynamicUser`, nur Gruppe `bird`, keine Capabilities

## API

- `GET /api/stream`: Server-Sent Events, Gesamtzustand pro Intervall
- `GET /api/state`: Gesamtzustand einmalig
- `GET /api/local`: nur dieser Router (wird von `-remote` genutzt)
- `GET /api/route?mode=for|as&q=…&node=…`

## Lokal testen ohne BIRD

```bash
python3 testdata/fakebird.py /tmp/bird.ctl spare-1 172.20.15.129 &
./dn42-lg -socket /tmp/bird.ctl -prefixes 172.20.15.128/27
```
