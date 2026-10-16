#!/usr/bin/env bash
# Writes to <file> the notes of a version for the release job of ci.yml: its section in CHANGELOG.md, what follows
# its `## vX.Y.Z` title up to the next one. Fails when there is none.
#
# CHANGELOG.md is Markdown, whose line breaks inside a paragraph are spaces; GitHub keeps them in the text of a
# release. So the lines of each paragraph, list item and quote are joined, but for a hard break (a line ending with
# a backslash or two spaces). Code blocks, headings, tables and HTML are left as they are.
#
#   .github/scripts/release-notes.sh <version> <file>
set -euo pipefail

root="$(cd "$(dirname "$0")/../.." && pwd)"
version="$1"
notes="$2"

# The lines that join_lines tells apart
fence='^ *(```|~~~)'                 # opens or closes a code block
blank='^[[:space:]]*>?[[:space:]]*$' # a blank line, in a quote too
block='^ *(#|[|]|<)'                 # a heading, a table row, HTML
rule='^ *(-+|[*]+|_+|=+) *$'         # a thematic break, or the underline of a heading
item='^ *([-*+]|[0-9]+[.)])( |$)'    # a list item
quote='^ *> *(.*)$'                  # a line of a quote, and its text
text='^[[:space:]]*(.*)$'            # a line, and its text without its indentation
hard_break='(  |[\])$'               # the end of a line that keeps its break

# section prints the section of the version.
section() {
  local line title found=false
  # || [[ -n ]]: the last line too, when the file does not end with a line break
  while IFS= read -r line || [[ -n "$line" ]]; do
    if [[ "$line" == "## "* ]]; then
      read -r _ title _ <<<"$line"
      if [[ "$title" == "$version" ]]; then found=true; else found=false; fi
    elif $found; then
      printf '%s\n' "$line"
    fi
  done <"$root/CHANGELOG.md"
}

# join_lines prints its input with the lines of each paragraph, list item and quote joined. held is the one being
# joined: it is printed once it ends. BASH_REMATCH is what the last =~ of a test matched: the text of line.
join_lines() {
  local line held="" fenced=false
  while IFS= read -r line; do
    if [[ "$line" =~ $fence ]]; then
      # A code block, its fences included: as it is
      flush
      if $fenced; then fenced=false; else fenced=true; fi
      printf '%s\n' "$line"
    elif $fenced; then
      printf '%s\n' "$line"
    elif [[ "$line" =~ $blank || "$line" =~ $block || "$line" =~ $rule ]]; then
      # On a line of its own
      flush
      printf '%s\n' "$line"
    elif [[ "$held" =~ $quote && ! "$held" =~ $hard_break && "$line" =~ $quote ]]; then
      # Goes on the previous line of the same quote
      held+=" ${BASH_REMATCH[1]}"
    elif [[ "$line" =~ $item || "$line" =~ $quote ]]; then
      # Starts a new line
      flush
      held="$line"
    elif [[ -n "$held" && ! "$held" =~ $hard_break && "$line" =~ $text ]]; then
      # Goes on the held line
      held+=" ${BASH_REMATCH[1]}"
    else
      flush
      held="$line"
    fi
  done
  flush
}

# flush prints the line that join_lines holds, if any: held is never an empty line.
flush() {
  if [[ -n "$held" ]]; then
    printf '%s\n' "$held"
  fi
  held=""
}

section | join_lines >"$notes"
if ! grep -q '[^[:space:]]' "$notes"; then
  echo "::error file=CHANGELOG.md::No section for $version (expected a '## $version' title followed by what the version changes)"
  exit 1
fi
