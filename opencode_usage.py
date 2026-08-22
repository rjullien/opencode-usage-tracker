#!/usr/bin/env python3
"""opencode_usage.py — Suivi des quotas serveur OpenCode (rolling 5h / weekly / monthly).

Récupère les limites serveur depuis la page de workspace OpenCode en scrappant
le state SSR embarqué (seule voie tant que l'API publique n'existe pas, issue #31084).

Usage:
  python3 opencode_usage.py --workspace-id <ID> --cookie "auth=Fe26.2*..."
  python3 opencode_usage.py --workspace-id <ID> --cookie-file ~/.config/opencode/cookies.txt
  python3 opencode_usage.py --workspace-id <ID>            # auto-détection cookie Firefox
  python3 opencode_usage.py --workspace-id <ID> --json     # sortie JSON pour scripting/cron

Env:
  OPENCODE_WORKSPACE_ID  — id de workspace si --workspace-id absent
  OPENCODE_COOKIE        — valeur brute du cookie auth si --cookie absent

Sortie:
  JSON (--json) : {"rolling": {...}, "weekly": {...}, "monthly": {...}, "plan": "..."}
  Texte (défaut) : barres colorées rolling/weekly/monthly avec % et temps avant reset.
"""
from __future__ import annotations

import argparse
import json
import os
import re
import sqlite3
import sys
import urllib.request
from datetime import datetime, timedelta, timezone
from pathlib import Path

BASE_URL = "https://opencode.ai/workspace/{wid}/go"
COOKIE_RE = re.compile(r"^auth=Fe26\.2")


def log_err(msg: str) -> None:
    print(f"[opencode-usage] {msg}", file=sys.stderr)


def load_cookie_arg(raw: str) -> str:
    """--cookie 'auth=...' ou --cookie 'Fe26.2...' → envoie 'auth=Fe26.2...'."""
    raw = raw.strip().strip('"').strip("'")
    if raw.startswith("auth="):
        return raw
    return f"auth={raw}"


def load_cookie_file(path: str) -> str:
    """Fichier cookies : soit format Netscape (domaine\\t...\\tauth\\tvaleur),
    soit ligne simple `auth=Fe26.2*...` — les deux sont acceptés."""
    p = Path(path).expanduser()
    if not p.exists():
        raise FileNotFoundError(f"fichier cookie introuvable: {p}")
    lines = p.read_text(encoding="utf-8", errors="replace").splitlines()
    # format simple ligne par ligne
    for line in lines:
        line = line.strip()
        if line.lower().startswith("auth="):
            return load_cookie_arg(line.split("=", 1)[1])
    # format Netscape
    candidate = None
    for line in lines:
        if not line or line.startswith("#"):
            continue
        parts = line.split("\t")
        if len(parts) >= 7 and "opencode.ai" in parts[0] and parts[5] == "auth":
            candidate = parts[6]
    if candidate is None:
        raise ValueError(f"aucun cookie 'auth' opencode.ai trouvé dans {p}")
    return f"auth={candidate}"


def find_firefox_cookie() -> str:
    """Auto-détection du cookie auth dans le cookies.sqlite de Firefox (profil par défaut)."""
    profiles = sorted(Path.home().glob(".mozilla/firefox/*/cookies.sqlite"))
    if not profiles:
        raise FileNotFoundError("aucun profil Firefox trouvé (~/.mozilla/firefox)")
    for db in profiles:
        try:
            con = sqlite3.connect(f"file:{db}?mode=ro&immutable=1", uri=True)
            row = con.execute(
                "SELECT value FROM moz_cookies WHERE host LIKE '%opencode.ai%' AND name='auth' ORDER BY last_accessed DESC LIMIT 1"
            ).fetchone()
            con.close()
            if row:
                return f"auth={row[0]}"
        except Exception:
            continue
    raise ValueError("cookie opencode.ai 'auth' absent des profils Firefox")


def fetch_page(wid: str, cookie: str) -> str:
    req = urllib.request.Request(BASE_URL.format(wid=wid), headers={
        "User-Agent": "Mozilla/5.0 (X11; Linux x86_64; rv:128.0) Gecko/20100101 Firefox/128.0",
        "Accept": "text/html,application/xhtml+xml",
        "Cookie": cookie,
    })
    try:
        with urllib.request.urlopen(req, timeout=30) as resp:
            return resp.read().decode("utf-8", errors="replace")
    except urllib.error.HTTPError as e:
        if e.code in (401, 403):
            raise RuntimeError(f"HTTP {e.code} — cookie invalide ou expiré (ré-authentifie sur opencode.ai)")
        raise RuntimeError(f"HTTP {e.code} — {e.reason}")
    except urllib.error.URLError as e:
        raise RuntimeError(f"réseau: {e.reason}")


def parse_state(html: str) -> dict:
    """Extrait le state SSR (rolling/weekly/monthly + plan) du <script> embarqué.
    Tolérant aux variations de format : cherche les blocs par mots-clés."""
    # 1) tous les contenus de <script> qui mentionnent rolling ou weekly
    scripts = re.findall(r"<script[^>]*>(.*?)</script>", html, flags=re.S | re.I)
    blob = ""
    for s in scripts:
        if re.search(r"rolling|weekly|monthly|reset_in", s, flags=re.I):
            blob = s
            break
    if not blob:
        raise ValueError("state SSR introuvable dans la page (structure opencode.ai modifiée ?)")

    def grab(pattern: str) -> str | None:
        m = re.search(pattern, blob, flags=re.I)
        return m.group(1).strip() if m else None

    def num(val: str | None) -> int | None:
        if val is None:
            return None
        digits = re.sub(r"[^0-9.]", "", val)
        return int(float(digits)) if digits else None

    out: dict = {"rolling": {}, "weekly": {}, "monthly": {}, "plan": None}
    for key, aliases in (
        ("rolling", ["rolling", "rolling5h", "rolling_5h"]),
        ("weekly", ["weekly", "week"]),
        ("monthly", ["monthly", "month"]),
    ):
        perc = None
        for a in aliases:
            perc = num(grab(rf'"{a}"[^}}]*?"percent"?\s*:\s*([0-9.]+)'))
            if perc is not None:
                break
        # fallback: motif "percent":V à proximité du mot-clé
        if perc is None:
            m = re.search(rf'{aliases[0]}[^}}]{{0,300}}?percent"?\s*:\s*([0-9.]+)', blob, flags=re.I)
            perc = num(m.group(1)) if m else None
        out[key] = {
            "percent": perc,
            "reset_in_sec": num(grab(rf'reset_in_sec"?\s*:\s*([0-9.]+)')),  # dernier reset vu = monthly
        }

    # timers de reset : on prend les N derniers reset_in_sec rencontrés dans l'ordre
    resets = [num(x) for x in re.findall(r'reset_?in_?sec"?\s*:\s*([0-9.]+)', blob, flags=re.I) if num(x)]
    if len(resets) >= 1:
        out["rolling"]["reset_in_sec"] = resets[0]
    if len(resets) >= 2:
        out["weekly"]["reset_in_sec"] = resets[1]
    if len(resets) >= 3:
        out["monthly"]["reset_in_sec"] = resets[2]

    m = re.search(r'"plan"?\s*:\s*"([^"]+)"', blob)
    out["plan"] = m.group(1) if m else None
    return out


def fmt_reset(sec: int | None) -> str:
    if sec is None:
        return "?"
    now = datetime.now(timezone.utc)
    d = timedelta(seconds=sec)
    target = now + d
    return target.strftime("%a %d/%m %H:%M")


def bar(percent: int | None, width: int = 24) -> str:
    if percent is None:
        return " " * width + "  n/a"
    n = round(percent / 100 * width)
    filled = "█" * n
    empty = "░" * (width - n)
    return f"{filled}{empty} {percent:>3}%"


def color(percent: int | None) -> str:
    if percent is None:
        return "\033[90m"  # gris
    if percent >= 90:
        return "\033[91m"  # rouge
    if percent >= 70:
        return "\033[93m"  # jaune
    return "\033[92m"  # vert


def main() -> int:
    ap = argparse.ArgumentParser(description="Quotas serveur OpenCode (scrape state SSR)")
    ap.add_argument("--workspace-id", "-w", help="ID du workspace opencode.ai")
    ap.add_argument("--cookie", "-c", help="valeur du cookie auth (ou 'auth=...')")
    ap.add_argument("--cookie-file", "-f", help="fichier cookies.txt (format Netscape)")
    ap.add_argument("--json", action="store_true", help="sortie JSON brut")
    args = ap.parse_args()

    wid = args.workspace_id or os.environ.get("OPENCODE_WORKSPACE_ID")
    if not wid:
        log_err("manque --workspace-id (ou env OPENCODE_WORKSPACE_ID)")
        return 2

    try:
        if args.cookie:
            cookie = load_cookie_arg(args.cookie)
        elif args.cookie_file:
            cookie = load_cookie_file(args.cookie_file)
        elif os.environ.get("OPENCODE_COOKIE"):
            cookie = load_cookie_arg(os.environ["OPENCODE_COOKIE"])
        else:
            cookie = find_firefox_cookie()
    except (FileNotFoundError, ValueError) as e:
        log_err(str(e))
        return 2

    try:
        html = fetch_page(wid, cookie)
        state = parse_state(html)
    except (RuntimeError, ValueError) as e:
        log_err(str(e))
        return 1

    if args.json:
        print(json.dumps(state, ensure_ascii=False))
        return 0

    print(f"📊 Quotas OpenCode — plan: {state['plan'] or '?'} | workspace {wid}")
    for key, label in (("rolling", "⏱ rolling 5h "), ("weekly", "📅 weekly    "), ("monthly", "🗓 monthly   ")):
        s = state[key]
        p = s.get("percent")
        print(f"{label} \033[90m|{color(p)}{bar(p)}\033[0m\033[90m| reset: {fmt_reset(s.get('reset_in_sec'))}\033[0m")
    return 0


if __name__ == "__main__":
    sys.exit(main())