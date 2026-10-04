#!/usr/bin/env python3
"""Export self-contained, offline previews with simulated data only."""
from pathlib import Path

root = Path(__file__).resolve().parent.parent
out = root / 'design-previews'
out.mkdir(exist_ok=True)
html = (root / 'web/static/console.html').read_text()
css = (root / 'web/static/style.css').read_text()
js = (root / 'web/static/app.js').read_text()
for name, theme in [('studio', 'studio'), ('signal', 'terminal'), ('fleet', 'fleet')]:
    script = js.replace("demo=params.get('demo')==='1'", 'demo=true')
    script = script.replace("params.get('design')||'terminal'", repr(theme))
    script = script.replace("localStorage.setItem('lidza-design',design)", 'void 0')
    page = html.replace('<link rel="stylesheet" href="/style.css">', '<style>' + css + '</style>')
    page = page.replace('<script src="/app.js" defer></script>', '')
    page = page.replace('</body>', '<script>' + script + '</script></body>')
    page = page.replace('href="/"', 'href="signal.html"').replace('href="/console.html"', 'href="signal.html"').replace('href="/designs.html"', 'href="studio.html"')
    page = page.replace("location.href='/';return", "location.href='signal.html';return")
    (out / (name + '.html')).write_text(page)
