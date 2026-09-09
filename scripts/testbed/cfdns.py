#!/usr/bin/env python3
"""DNS names for the testbed VMs in a Cloudflare zone.

    cfdns.py up   <name> <ip> [<name> <ip> ...]   create or update A records
    cfdns.py down                                  remove every record under the testbed label
    cfdns.py list                                  show what is there

Each VM gets <name>.<label>.<zone> and *.<name>.<label>.<zone> (any site name
under the VM resolves), DNS-only: the VMs sit on a private network, nothing to
proxy. Settings come from the environment: TB_ZONE (the zone, e.g. example.com),
TB_DNS_LABEL (default "tb") and TB_CF_TOKEN (an API token with Zone:DNS:Edit on
that zone; it never appears in the output).
"""
import json
import os
import sys
import urllib.error
import urllib.request

API = "https://api.cloudflare.com/client/v4"


def die(msg):
    print("cfdns: " + msg, file=sys.stderr)
    sys.exit(1)


def call(method, path, body=None):
    data = json.dumps(body).encode() if body is not None else None
    req = urllib.request.Request(API + path, data=data, method=method, headers={
        "Authorization": "Bearer " + os.environ["TB_CF_TOKEN"], "Content-Type": "application/json"})
    try:
        with urllib.request.urlopen(req, timeout=30) as res:
            out = json.load(res)
    except urllib.error.HTTPError as e:
        try:
            errors = json.load(e)["errors"]
        except Exception:  # noqa: BLE001 - the body is only decoration for the message
            errors = []
        die(f"{method} {path}: HTTP {e.code} " + "; ".join(f"{x['code']} {x['message']}" for x in errors))
    if not out.get("success"):
        die(f"{method} {path}: " + "; ".join(f"{x['code']} {x['message']}" for x in out.get("errors", [])))
    return out["result"]


def main():
    for v in ("TB_ZONE", "TB_CF_TOKEN"):
        if not os.environ.get(v):
            die(f"{v} is not set")
    zone_name = os.environ["TB_ZONE"]
    label = os.environ.get("TB_DNS_LABEL", "tb")
    suffix = f".{label}.{zone_name}"
    zones = call("GET", f"/zones?name={zone_name}")
    if not zones:
        die(f"zone {zone_name} is not visible to this token")
    zone = zones[0]["id"]
    existing = {r["name"]: r for r in call("GET", f"/zones/{zone}/dns_records?type=A&per_page=500")
                if r["name"].endswith(suffix)}
    cmd = sys.argv[1] if len(sys.argv) > 1 else "list"
    if cmd == "list":
        for name, r in sorted(existing.items()):
            print(f"{name} -> {r['content']}")
        return
    if cmd == "down":
        for name, r in sorted(existing.items()):
            call("DELETE", f"/zones/{zone}/dns_records/{r['id']}")
            print(f"removed {name}")
        return
    if cmd != "up" or len(sys.argv) < 4 or len(sys.argv) % 2:
        die("usage: cfdns.py up <name> <ip> [...] | down | list")
    pairs = list(zip(sys.argv[2::2], sys.argv[3::2]))
    for vm, ip in pairs:
        for name in (f"{vm}{suffix}", f"*.{vm}{suffix}"):
            body = {"type": "A", "name": name, "content": ip, "ttl": 300, "proxied": False,
                    "comment": "MonoPanel testbed"}
            if name in existing:
                if existing[name]["content"] != ip:
                    call("PATCH", f"/zones/{zone}/dns_records/{existing[name]['id']}", body)
                    print(f"updated {name} -> {ip}")
                else:
                    print(f"kept    {name} -> {ip}")
            else:
                call("POST", f"/zones/{zone}/dns_records", body)
                print(f"created {name} -> {ip}")


if __name__ == "__main__":
    main()
