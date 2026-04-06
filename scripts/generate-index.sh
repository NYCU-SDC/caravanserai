#!/usr/bin/env bash
#
# generate-index.sh — Build an index.html that links to all generated schema doc pages.
#
# Usage: scripts/generate-index.sh <output-dir>
#   e.g.  scripts/generate-index.sh website/out
#
# Only top-level schema pages (e.g. node.md, project.md) are linked.
# Sub-definition pages (e.g. node-defs-condition.md) are excluded.

set -euo pipefail

OUT_DIR="${1:?Usage: $0 <output-dir>}"

cat > "${OUT_DIR}/index.html" <<'HTML_HEAD'
<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="UTF-8">
  <meta name="viewport" content="width=device-width, initial-scale=1.0">
  <title>Caravanserai API Schema Documentation</title>
  <style>
    body {
      font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, Helvetica, Arial, sans-serif;
      max-width: 720px;
      margin: 2rem auto;
      padding: 0 1rem;
      color: #24292e;
      line-height: 1.6;
    }
    h1 { border-bottom: 1px solid #e1e4e8; padding-bottom: 0.5rem; }
    ul { list-style: none; padding: 0; }
    li { margin: 0.75rem 0; }
    a {
      color: #0366d6;
      text-decoration: none;
      font-size: 1.1rem;
      font-weight: 500;
    }
    a:hover { text-decoration: underline; }
    footer {
      margin-top: 3rem;
      color: #586069;
      font-size: 0.85rem;
      border-top: 1px solid #e1e4e8;
      padding-top: 1rem;
    }
  </style>
</head>
<body>
  <h1>Caravanserai API Schema Documentation</h1>
  <p>Resource type definitions for the Caravanserai container orchestration platform.</p>
  <ul>
HTML_HEAD

# Only include top-level schema pages (no hyphens in basename = not a sub-definition)
for page in "${OUT_DIR}"/*.md; do
  filename="$(basename "$page")"
  # Skip sub-definition pages like node-defs-condition.md
  case "$filename" in *-*) continue ;; esac
  name="${filename%.md}"
  title="$(echo "$name" | sed 's/.*/\u&/')"
  echo "    <li><a href=\"${filename}\">${title}</a></li>" >> "${OUT_DIR}/index.html"
done

cat >> "${OUT_DIR}/index.html" <<'HTML_TAIL'
  </ul>
  <footer>
    Generated from <a href="https://github.com/NYCU-SDC/caravanserai/tree/main/schemas">JSON Schema sources</a>
    by <a href="https://github.com/adobe/jsonschema2md">jsonschema2md</a>.
  </footer>
</body>
</html>
HTML_TAIL
