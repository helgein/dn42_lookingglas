#!/usr/bin/env python3
"""Minimaler BIRD-Control-Socket-Simulator zum Testen von dn42-lg.

Aufruf: fakebird.py /tmp/bird1.ctl spare-1 172.20.15.129
"""
import os, random, socket, sys, threading, time

path, host, rid = sys.argv[1], sys.argv[2], sys.argv[3]
OWN = 4242423236
PEERS = [("dn42_kioubit", 4242423914), ("dn42_lare", 4242423035), ("dn42_iedon", 4242422189)]
start = time.time()
cnt = {p: [random.randint(40000, 90000), random.randint(1000, 3000)] for p, _ in PEERS}
lock = threading.Lock()


def bump():
    while True:
        time.sleep(1)
        with lock:
            for p in cnt:
                if random.random() < 0.6:
                    cnt[p][0] += random.choice([0, 1, 2, 5, 20, 80])
                    cnt[p][1] += random.choice([0, 0, 1, 3])


def channel(name, table, imp, exp, upd, wdr):
    return [
        f"  Channel {name}",
        "    State:          UP",
        f"    Table:          {table}",
        "    Preference:     100",
        "    Input filter:   (unnamed)",
        "    Output filter:  (unnamed)",
        f"    Routes:         {imp} imported, {exp} exported, {imp} preferred",
        "    Route change stats:     received   rejected   filtered    ignored   RX limit      limit   accepted",
        f"      Import updates:     {upd:10d}          0          0         12          0          0 {upd-12:10d}",
        f"      Import withdraws:   {wdr:10d}          0        ---          3        ---        --- {wdr-3:10d}",
        f"      Export updates:     {upd*2:10d}     {upd:6d}          0        ---        ---        --- {upd//3:10d}",
        f"      Export withdraws:   {wdr:10d}        ---        ---        ---        ---        --- {wdr//3:10d}",
    ]


def protocols():
    out = [("2002", "Name       Proto      Table      State  Since         Info")]
    out.append(("1002", "device1    Device     ---        up     2026-09-29    "))
    out.append(("1002", "kernel1    Kernel     master4    up     2026-09-29    "))
    with lock:
        for name, asn in PEERS:
            up = not (name == "dn42_iedon" and int(time.time() - start) % 40 > 30)
            st = "up" if up else "start"
            info = "Established" if up else "Connect       Socket: Connection refused"
            out.append(("1002", f"{name:<10} BGP        ---        {st:<6} 2026-09-30    {info}"))
            det = [
                f"  BGP state:          {'Established' if up else 'Connect'}",
                f"    Neighbor address: fe80::{asn % 10000}%{name.replace('_', '-')}",
                f"    Neighbor AS:      {asn}",
                f"    Local AS:         {OWN}",
            ]
            if up:
                upd, wdr = cnt[name]
                det += channel("ipv4", "master4", 1300 + upd % 97, 2, upd, wdr)
                det += channel("ipv6", "master6", 1250 + upd % 89, 2, upd // 2, wdr // 2)
            out.append(("1006", "\n".join(det)))
        other = "runkel-eval" if host == "spare-1" else "spare-1"
        out.append(("1002", f"ibgp_{other.replace('-', '_'):<10} BGP  ---  up  2026-09-30    Established"))
        det = [
            "  BGP state:          Established",
            "    Neighbor address: fe80::3236:2%dn42-int",
            f"    Neighbor AS:      {OWN}",
            f"    Local AS:         {OWN}",
        ] + channel("ipv4", "master4", 4000, 4100, 9000, 100) + channel("ipv6", "master6", 3900, 4000, 8000, 90)
        out.append(("1006", "\n".join(det)))
    out.append(("0000", ""))
    return out


ROUTE = """Table master4:
172.20.0.53/32       unicast [dn42_kioubit 2026-09-30] * (100) [AS4242420053i]
	via fe80::ade0 on dn42-kioubit
	Type: BGP univ
	BGP.origin: IGP
	BGP.as_path: 4242423914 4242420053
	BGP.next_hop: fe80::ade0
	BGP.local_pref: 100
                     unicast [ibgp_runkel_eval 2026-09-30] (100) [AS4242420053i]
	via fe80::3236:2 on dn42-int
	BGP.as_path: 4242423035 4242420053"""


def respond(cmd):
    if cmd == "show protocols all":
        return protocols()
    if cmd == "show status":
        return [("1000", "BIRD 2.17.5"), ("1011", f"Router ID is {rid}"),
                ("1011", "Current server time is 2026-10-02 13:00:00.000"),
                ("1011", "Last reboot on 2026-09-29 10:00:00.000"),
                ("1011", "Last reconfiguration on 2026-10-01 13:45:36.023"),
                ("0013", "Daemon is up and running")]
    if cmd.startswith("show route ") and " export " in cmd:
        pfx = cmd.split()[2]
        if "iedon" in cmd and random.random() < 0.5:
            return [("8001", "Network not found")]
        return [("1007", f"{pfx}  unreachable [static1 2026-09-29] * (200)"), ("0000", "")]
    if cmd.startswith("show route for "):
        if "172.20.0.53" in cmd:
            return [(("1007" if i == 1 else "1008"), l) for i, l in enumerate(ROUTE.split("\n"))] + [("0000", "")]
        return [("8001", "Network not found")]
    if cmd.startswith("show route table") and "bgp_path" in cmd:
        return [("1007", "172.20.0.53/32       unicast [dn42_kioubit 2026-09-30] * (100) [AS4242420053i]"),
                ("1007", "172.22.0.0/24        unicast [dn42_lare 2026-09-30] * (100) [AS4242423035i]"), ("0000", "")]
    if cmd.startswith("configure") or cmd.startswith("disable"):
        return [("8007", "Access denied")]
    return [("9001", "syntax error")]


def send(conn, lines):
    buf = []
    for i, (code, text) in enumerate(lines):
        last = i == len(lines) - 1
        parts = text.split("\n")
        for j, part in enumerate(parts):
            if j == 0:
                buf.append(f"{code}{' ' if last and len(parts) == 1 else '-'}{part}")
            else:
                buf.append(" " + part)
        if last and len(parts) > 1:
            buf.append(f"{code} ")
    conn.sendall(("\n".join(buf) + "\n").encode())


def handle(conn):
    with conn:
        send(conn, [("0001", "BIRD 2.17.5 ready.")])
        f = conn.makefile("r")
        restricted = False
        for line in f:
            cmd = line.strip()
            if cmd == "restrict":
                restricted = True
                send(conn, [("0016", "Access restricted")])
                continue
            if not restricted:
                send(conn, [("9001", "test: restrict expected first")])
                continue
            send(conn, respond(cmd))


if os.path.exists(path):
    os.unlink(path)
srv = socket.socket(socket.AF_UNIX)
srv.bind(path)
srv.listen(16)
threading.Thread(target=bump, daemon=True).start()
while True:
    c, _ = srv.accept()
    threading.Thread(target=handle, args=(c,), daemon=True).start()
