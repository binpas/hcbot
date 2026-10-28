#!/bin/sh
# Downloads the comic art pack for /comic into a folder (the bot's
# COMIC_ART_DIR, for example /mnt/POOL/apps/hcbot/comic):
#
#   deploy/comic-art.sh /mnt/POOL/apps/hcbot/comic
#
# The art is Microsoft Comic Chat's (characters by Jim Woodring), from
# Microsoft's open-source release under the MIT license, as sprite sheets
# made by comic-chat-web (https://github.com/remsky/comic-chat-web). The
# commit is fixed. Only the backgrounds with Microsoft's copyright are
# used. The license files go in the folder too.
#
# Needs curl and python3. Run it as the owner of the folder (or chown it
# after).
set -eu

COMMIT=c18a468909c0a0dacb0e6aa3ceb8c757b66370c9
BASE="https://raw.githubusercontent.com/remsky/comic-chat-web/$COMMIT"

[ $# -eq 1 ] || { echo "usage: $0 <folder>" >&2; exit 2; }
dest=$1
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
mkdir -p "$tmp/avatars" "$tmp/backgrounds"

get() { curl -fsSL --retry 3 -o "$2" "$BASE/$1"; }

get public/assets/avatars/manifest.json "$tmp/avatars/manifest.json"
get public/assets/backgrounds/manifest.json "$tmp/backgrounds/all.json"
get LICENSES/MIT-Microsoft.txt "$tmp/LICENSE-Microsoft.txt"
get THIRD_PARTY_NOTICES.md "$tmp/THIRD_PARTY_NOTICES.md"

# The sprite sheets named in the manifest.
python3 - "$tmp/avatars/manifest.json" > "$tmp/sheets.txt" <<'EOF'
import json, os, re, sys
m = json.load(open(sys.argv[1]))
names = {os.path.basename(p["sprite"]["atlasUrl"]) for a in m["avatars"] for p in a["poses"]}
for n in sorted(names):
    if not re.fullmatch(r"[a-z0-9_-]+\.png", n):
        sys.exit("bad sheet name: " + n)
    print(n)
EOF
while read -r sheet; do
	get "public/assets/avatars/$sheet" "$tmp/avatars/$sheet"
done < "$tmp/sheets.txt"

# Only the backgrounds with Microsoft's copyright.
python3 - "$tmp/backgrounds/all.json" "$tmp/backgrounds/manifest.json" > "$tmp/backs.txt" <<'EOF'
import json, os, re, sys
m = json.load(open(sys.argv[1]))
keep = [b for b in m["backdrops"] if "Microsoft" in (b.get("copyright") or "")]
json.dump({"backdrops": keep}, open(sys.argv[2], "w"), indent=1)
for b in keep:
    n = os.path.basename(b["url"])
    if not re.fullmatch(r"[a-z0-9_-]+\.png", n):
        sys.exit("bad background name: " + n)
    print(n)
EOF
rm "$tmp/backgrounds/all.json"
while read -r back; do
	get "public/assets/backgrounds/$back" "$tmp/backgrounds/$back"
done < "$tmp/backs.txt"
rm "$tmp/sheets.txt" "$tmp/backs.txt"

cat > "$tmp/README.txt" <<EOF
Comic art for the HEALTH bot (/comic), from comic-chat-web commit $COMMIT.
Microsoft Comic Chat art, MIT license (LICENSE-Microsoft.txt). The
characters were illustrated by Jim Woodring. See THIRD_PARTY_NOTICES.md.
EOF

mkdir -p "$dest"
cp -R "$tmp/." "$dest/"
echo "Installed $(ls "$tmp/avatars" | grep -c '\.png$') sprite sheets and $(ls "$tmp/backgrounds" | grep -c '\.png$') backgrounds in $dest"
