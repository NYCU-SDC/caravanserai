#!/usr/bin/env bash
#
# md-to-html.sh — Convert all Markdown files in a directory to styled HTML pages.
#
# Usage: scripts/md-to-html.sh <dir>
#   e.g.  scripts/md-to-html.sh website/out
#
# Requires: npx marked (installed via Node.js)

set -euo pipefail

DIR="${1:?Usage: $0 <directory>}"

HTML_HEAD='<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="UTF-8">
  <meta name="viewport" content="width=device-width, initial-scale=1.0">
  <title>%TITLE% — Caravanserai</title>
  <style>
    body {
      font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, Helvetica, Arial, sans-serif;
      max-width: 960px;
      margin: 2rem auto;
      padding: 0 1.5rem;
      color: #24292e;
      line-height: 1.6;
    }
    nav { margin-bottom: 1.5rem; font-size: 0.9rem; }
    nav a { color: #0366d6; text-decoration: none; }
    nav a:hover { text-decoration: underline; }
    h1, h2, h3 { margin-top: 1.5em; }
    h1 { border-bottom: 1px solid #e1e4e8; padding-bottom: 0.5rem; }
    table { border-collapse: collapse; width: 100%; margin: 1rem 0; }
    th, td { border: 1px solid #e1e4e8; padding: 0.5rem 0.75rem; text-align: left; }
    th { background: #f6f8fa; }
    code { background: #f6f8fa; padding: 0.15rem 0.4rem; border-radius: 3px; font-size: 0.9em; }
    pre { background: #f6f8fa; padding: 1rem; border-radius: 6px; overflow-x: auto; }
    pre code { background: none; padding: 0; }
    a { color: #0366d6; text-decoration: none; }
    a:hover { text-decoration: underline; }
    blockquote { border-left: 4px solid #dfe2e5; margin: 0; padding: 0.5rem 1rem; color: #6a737d; }
  </style>
</head>
<body>
  <nav><a href="index.html">&larr; Back to index</a></nav>'

HTML_TAIL='</body>
</html>'

for md_file in "${DIR}"/*.md; do
  [ -f "$md_file" ] || continue

  filename="$(basename "$md_file")"
  html_file="${DIR}/${filename%.md}.html"

  # Extract title from first heading, fall back to filename
  title="$(head -5 "$md_file" | sed -n 's/^# *//p' | head -1)"
  [ -z "$title" ] && title="${filename%.md}"

  # Convert and wrap; rewrite internal .md links to .html
  body="$(npx --yes marked --gfm "$md_file" | sed 's/\.md"/\.html"/g; s/\.md#/\.html#/g')"
  head_with_title="${HTML_HEAD//%TITLE%/$title}"

  printf '%s\n%s\n%s\n' "$head_with_title" "$body" "$HTML_TAIL" > "$html_file"
done

echo "Converted $(ls "${DIR}"/*.html 2>/dev/null | wc -l) HTML files in ${DIR}/"
