#!/bin/sh
# Set up one bot instance: its user, group and working folder, and its
# systemd unit made from deploy/systemd/gohealthy.service.in. The Makefile
# runs this; see `make help`.
#
#   instance.sh NAME            ask for the settings, save them, make the unit
#   instance.sh --render NAME   make the unit again from the saved settings
#   instance.sh --install NAME  install the unit and the folder (as root)
#
# Settings are saved in instances/NAME.conf. BOTUSER, BOTGROUP and RUNDIR
# in the environment override them. BINDIR comes from the Makefile.

set -eu

cd "$(dirname "$0")/.."

mode=ask
case ${1:-} in
--render | --install) mode=${1#--}; shift ;;
esac
NAME=${1:-}

die() { echo "instance: $*" >&2; exit 1; }

case $NAME in
"" | -* | *[!A-Za-z0-9_.-]*) die "NAME must be letters, digits, '.', '_' or '-' (got '$NAME')." ;;
esac

conf=instances/$NAME.conf
unit=build/$NAME.service

# Saved settings, then the environment.
env_user=${BOTUSER:-} env_group=${BOTGROUP:-} env_rundir=${RUNDIR:-}
BOTUSER="" BOTGROUP="" RUNDIR=""
if [ -f "$conf" ]; then
	BOTUSER=$(sed -n 's/^BOTUSER=//p' "$conf")
	BOTGROUP=$(sed -n 's/^BOTGROUP=//p' "$conf")
	RUNDIR=$(sed -n 's/^RUNDIR=//p' "$conf")
elif [ "$mode" != ask ]; then
	die "no settings for '$NAME'. Run: make instance NAME=$NAME"
fi
BOTUSER=${env_user:-$BOTUSER}
BOTGROUP=${env_group:-$BOTGROUP}
RUNDIR=${env_rundir:-$RUNDIR}

if [ "$mode" = ask ]; then
	prompt() { # prompt VAR "Question" default
		eval "cur=\${$1}"
		def=${cur:-$3}
		if [ -t 0 ]; then
			printf '%s [%s]: ' "$2" "$def"
			read -r reply || reply=""
			def=${reply:-$def}
		fi
		eval "$1=\$def"
	}
	prompt BOTUSER "User that runs the bot" gohealthy
	prompt BOTGROUP "Group" "$BOTUSER"
	prompt RUNDIR "Working folder (.env, databases, backups)" "/opt/$NAME"
fi

case $BOTUSER$BOTGROUP in
*[!A-Za-z0-9_.-]*) die "the user and group must be letters, digits, '.', '_' or '-'." ;;
esac
[ -n "$BOTUSER" ] && [ -n "$BOTGROUP" ] || die "the user and group must not be empty."
case $RUNDIR in
/*) ;;
*) die "the working folder must be an absolute path." ;;
esac
case $RUNDIR in
*[!A-Za-z0-9_./-]* | */ | / | *//* | */../* | */..) die "the working folder must be a simple path without spaces or a trailing '/' (got '$RUNDIR')." ;;
/root | /root/* | /run/user/*) die "the working folder cannot be in /root or /run/user." ;;
esac

render() {
	[ -n "${BINDIR:-}" ] || die "BINDIR is not set; run this from make."
	# A folder in /home needs its own view of /home: ProtectHome=yes hides
	# all of it (203/EXEC), and the tmpfs parent must be 0755 because
	# UMask=0077 would make it root-only (200/CHDIR).
	# awk -v turns each \n into a new line.
	case $RUNDIR in
	/home/*/*)
		top=/home/$(echo "${RUNDIR#/home/}" | cut -d/ -f1)
		protect="ProtectHome=tmpfs\nTemporaryFileSystem=$top:mode=0755\nBindPaths=$RUNDIR" ;;
	/home/*)
		protect="ProtectHome=tmpfs\nBindPaths=$RUNDIR" ;;
	*) protect="ProtectHome=yes" ;;
	esac
	mkdir -p build
	awk -v name="$NAME" -v user="$BOTUSER" -v group="$BOTGROUP" -v rundir="$RUNDIR" \
		-v bindir="$BINDIR" -v protect="$protect" '
		$0 == "@PROTECTHOME@" { print protect; next }
		{
			gsub(/@NAME@/, name); gsub(/@USER@/, user); gsub(/@GROUP@/, group)
			gsub(/@RUNDIR@/, rundir); gsub(/@BINDIR@/, bindir)
			print
		}' deploy/systemd/gohealthy.service.in >"$unit.tmp"
	mv "$unit.tmp" "$unit"
	echo "Made $unit (user $BOTUSER, folder $RUNDIR, binary $BINDIR/gohealthy)."
}

case $mode in
ask)
	mkdir -p instances
	printf 'BOTUSER=%s\nBOTGROUP=%s\nRUNDIR=%s\n' "$BOTUSER" "$BOTGROUP" "$RUNDIR" >"$conf"
	echo "Saved $conf."
	render
	echo "Next, on the server: sudo make install-instance NAME=$NAME"
	;;
render)
	render
	;;
install)
	[ "$(id -u)" = 0 ] || die "run this as root: sudo make install-instance NAME=$NAME"
	command -v systemctl >/dev/null 2>&1 || die "systemctl not found; install-instance is for a Linux server with systemd."
	[ -f "$unit" ] || die "$unit not found. Run: make instance NAME=$NAME (or make services)"
	id -u "$BOTUSER" >/dev/null 2>&1 || die "the user '$BOTUSER' does not exist. Make it first, for example:
  sudo useradd --system --home-dir $RUNDIR --shell /usr/sbin/nologin $BOTUSER"
	getent group "$BOTGROUP" >/dev/null 2>&1 || die "the group '$BOTGROUP' does not exist."
	[ -x "$BINDIR/gohealthy" ] || echo "Warning: $BINDIR/gohealthy is not installed yet. Run: sudo make install"

	target=/etc/systemd/system/$NAME.service
	if [ -f "$target" ] && ! cmp -s "$unit" "$target"; then
		echo "$target already exists and is different:"
		diff -u "$target" "$unit" || true
		if [ "${FORCE:-}" != 1 ]; then
			if [ -t 0 ]; then
				printf 'Replace it? [y/N]: '
				read -r reply || reply=""
				case $reply in [Yy]*) ;; *) die "no changes made." ;; esac
			else
				die "not replaced. Use FORCE=1 to replace it."
			fi
		fi
	fi

	if [ -d "$RUNDIR" ]; then
		echo "$RUNDIR already exists; its owner and mode are not changed."
	else
		install -d -o "$BOTUSER" -g "$BOTGROUP" -m 0700 "$RUNDIR"
		echo "Made $RUNDIR (owner $BOTUSER, mode 0700)."
	fi
	install -o "$BOTUSER" -g "$BOTGROUP" -m 0644 README.md "$RUNDIR/README.md"
	install -m 0644 "$unit" "$target"
	systemctl daemon-reload
	cat <<EOF
Installed $target.

Next steps:
  1. Put the bot's .env in $RUNDIR (its own BOT_TOKEN), private to $BOTUSER:
       sudo install -o $BOTUSER -g $BOTGROUP -m 0600 .env $RUNDIR/.env
  2. Start it now and at boot:  sudo systemctl enable --now $NAME
  3. Look at the log:           journalctl -u $NAME -f
EOF
	;;
esac
