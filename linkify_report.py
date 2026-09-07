# -*- coding: utf-8 -*-
"""Wrap every tender number mention in today's report with its registry link.
Sources the number->url map from daily/raw/records_2026-09-03.json.
Patches daily/2026-09-03.md and reports/2026-09-03.html, then syncs daily/2026-09-03.html."""
import json, re, shutil, sys
from pathlib import Path

ROOT = Path(__file__).parent
DAY = "2026-09-03"

def txt(v):
    if v is None: return ""
    if isinstance(v, list):
        return "".join(s.get("text", "") if isinstance(s, dict) else str(s) for s in v).strip()
    if isinstance(v, dict): return (v.get("text") or v.get("link") or "").strip()
    return str(v).strip()

items = json.loads((ROOT / "daily" / "raw" / f"records_{DAY}.json").read_text(encoding="utf-8"))
num2url = {}
for it in items:
    f = it.get("fields", {})
    n, u = txt(f.get("Номер")), txt(f.get("Ссылка"))
    if n and u:
        num2url[n] = u
print(f"map: {len(num2url)} numbers with url")

# longest first so nested numbers don't partially match
numbers = sorted(num2url, key=len, reverse=True)

def patch_text(text, link_fmt):
    """link_fmt: lambda num, url -> replacement string. Skips protected segments."""
    out, count = text, 0
    for n in numbers:
        url = num2url[n]
        # not preceded by [, /, =, word char; not followed by ]( or word char
        pat = re.compile(r"(?<![\w\[/=\(-])" + re.escape(n) + r"(?![\w\]/=\)])")
        out, c = pat.subn(lambda m: link_fmt(n, url), out)
        count += c
    return out, count

# ---------- Markdown ----------
md_path = ROOT / "daily" / f"{DAY}.md"
md = md_path.read_text(encoding="utf-8")
# protect existing markdown links [text](url) and bare urls from replacement
protected = []
def stash(m):
    protected.append(m.group(0))
    return f"\x00{len(protected)-1}\x00"
md_prot = re.sub(r"\[[^\]]*\]\([^)]*\)|https?://[^\s|)]+", stash, md)
md_prot, md_count = patch_text(md_prot, lambda n, u: f"[{n}]({u})")
md_new = re.sub(r"\x00(\d+)\x00", lambda m: protected[int(m.group(1))], md_prot)
md_path.write_text(md_new, encoding="utf-8")
print(f"md: {md_count} mentions linked")

# ---------- HTML ----------
html_path = ROOT / "reports" / f"{DAY}.html"
html = html_path.read_text(encoding="utf-8")
# protect tags, existing anchors, comments
protected = []
html_prot = re.sub(r"<a\b[^>]*>.*?</a>|<[^>]+>|<!--.*?-->", stash, html, flags=re.S)
html_prot, html_count = patch_text(
    html_prot,
    lambda n, u: f'<a href="{u}" target="_blank" rel="noopener">{n}</a>')
html_new = re.sub(r"\x00(\d+)\x00", lambda m: protected[int(m.group(1))], html_prot)
html_path.write_text(html_new, encoding="utf-8")
shutil.copy(html_path, ROOT / "daily" / f"{DAY}.html")
print(f"html: {html_count} mentions linked (reports/ + daily/ synced)")

# sanity: no leftover bare mentions of known numbers in md (outside links)
left = 0
prot2 = []
md_chk = re.sub(r"\[[^\]]*\]\([^)]*\)|https?://[^\s|)]+", lambda m: stash(m) or "", md_new)
protected.clear()
md_chk = re.sub(r"\[[^\]]*\]\([^)]*\)|https?://[^\s|)]+", "", md_new)
for n in numbers:
    if re.search(r"(?<![\w\[/=\(-])" + re.escape(n) + r"(?![\w\]/=\)])", md_chk):
        print(f"  LEFTOVER bare mention: {n}")
        left += 1
print(f"leftover bare mentions in md: {left}")
