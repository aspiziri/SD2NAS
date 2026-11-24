#!/bin/bash
# SD2NAS card script for Unassigned Devices (docs/PLAN.md §11, README.md).
#
# Install it as /boot/config/plugins/unassigned.devices/sd2nas.sh. Each slot of
# the card reader runs it from its own device script, a two-line wrapper:
#   #!/bin/bash
#   exec bash /boot/config/plugins/unassigned.devices/sd2nas.sh
# with Automount on, Read Only on, Share off and "Run Device Script File in the
# background" on. Unassigned Devices sets ACTION, DEVICE, LABEL, SERIAL and
# MOUNTPOINT. ACTION=NOTIFY
# (User Scripts, hourly) only delivers the notices SD2NAS has queued.
#
# Settings go in sd2nas.cfg (SD2NAS_CONF), so updating this script keeps them:
#   SD2NAS_URL=http://localhost:4040     # the container's host port
#
# Whether the card is safe to format comes from SD2NAS's reply; this script
# never decides it.

SD2NAS_CONF="${SD2NAS_CONF:-/boot/config/plugins/unassigned.devices/sd2nas.cfg}"
if [ -r "$SD2NAS_CONF" ]; then
	# shellcheck source=/dev/null
	. "$SD2NAS_CONF"
fi
# A cfg saved with Windows line endings leaves a CR on every value.
SD2NAS_URL="${SD2NAS_URL%$'\r'}"
SD2NAS_TOKEN="${SD2NAS_TOKEN%$'\r'}"
SD2NAS_NOTIFY="${SD2NAS_NOTIFY%$'\r'}"
SD2NAS_MAX_TIME="${SD2NAS_MAX_TIME%$'\r'}"
SD2NAS_URL="${SD2NAS_URL:-http://localhost:8080}"
SD2NAS_TOKEN="${SD2NAS_TOKEN:-}"
SD2NAS_NOTIFY="${SD2NAS_NOTIFY:-/usr/local/emhttp/webGui/scripts/notify}"
SD2NAS_MAX_TIME="${SD2NAS_MAX_TIME:-7260}" # SD2NAS answers by itself after 2 hours
PATH="$PATH:/usr/local/sbin:/usr/sbin:/sbin:/usr/bin:/bin"

# log TEXT: a syslog line (stderr if logger fails).
log() {
	logger -t sd2nas -- "$1" 2>/dev/null || printf 'sd2nas: %s\n' "$1" >&2
}

# notify LEVEL TEXT: an Unraid notification, plus a syslog line.
notify() {
	log "$1: $2"
	if ! "$SD2NAS_NOTIFY" -e "SD2NAS" -s "SD2NAS" -d "$2" -i "$1" </dev/null; then
		log "the notification above could not be sent"
	fi
}

# json_string TEXT: TEXT as a JSON string; control characters become spaces.
json_string() {
	local bs='\' q='"' s=$1
	s=${s//"$bs"/"$bs$bs"}
	s=${s//"$q"/"$bs$q"}
	s=${s//[[:cntrl:]]/ }
	printf '"%s"' "$s"
}

# post PATH MAX_TIME DATA: POST to SD2NAS. Sets CODE (the HTTP status, 000
# without a reply) and BODY; returns curl's exit status.
post() {
	local tmp rc auth=()
	CODE=000
	BODY=""
	if ! tmp=$(mktemp); then
		log "could not create a temporary file for the request to SD2NAS"
		return 1
	fi
	if [ -n "$SD2NAS_TOKEN" ]; then
		auth=(-H "Authorization: Bearer $SD2NAS_TOKEN")
	fi
	CODE=$(curl -q -sS --connect-timeout 10 --max-time "$2" -o "$tmp" -w '%{http_code}' \
		-X POST -H "X-SD2NAS: 1" -H "Content-Type: application/json" "${auth[@]}" \
		--data-binary "$3" "$SD2NAS_URL$1")
	rc=$?
	BODY=$(<"$tmp")
	rm -f "$tmp"
	return "$rc"
}

# server_reply: true when BODY is SD2NAS's two lines, a level and a message;
# sets LEVEL and MSG.
server_reply() {
	LEVEL=$(printf '%s\n' "$BODY" | sed -n 1p)
	MSG=$(printf '%s\n' "$BODY" | sed -n 2p)
	case "$LEVEL" in
		normal | warning | alert) [ -n "$MSG" ] ;;
		*) return 1 ;;
	esac
}

# ingest: import the card at MOUNTPOINT; unmount it only when SD2NAS answers
# 200, and pass SD2NAS's message on.
ingest() {
	local mp="${MOUNTPOINT%/}" card name uuid rc
	card="${mp##*/}"
	name="${LABEL:-$card}"
	name="${name:-$DEVICE}"
	name="${name//[[:cntrl:]]/ }"
	if [ -z "$card" ]; then
		notify alert "Unassigned Devices gave no mount point for $DEVICE, so the card was not imported."
		return
	fi
	if ! uuid=$(blkid -s UUID -o value "$DEVICE"); then
		log "could not read the filesystem UUID of $DEVICE"
		uuid=""
	fi
	post "/api/ingest?wait=1&format=text" "$SD2NAS_MAX_TIME" \
		"{\"path\":$(json_string "/cards/$card"),\"label\":$(json_string "$LABEL"),\"uuid\":$(json_string "$uuid")}"
	rc=$?
	if [ "$rc" -ne 0 ]; then
		log "import of $name failed: curl exit $rc"
	fi
	if [ "$rc" -eq 28 ]; then
		notify alert "SD2NAS did not finish importing $name in time. Card left mounted. Check SD2NAS."
	elif [ "$rc" -ne 0 ]; then
		notify alert "SD2NAS did not answer, so $name was not imported. Card left mounted. Is the container running?"
	elif ! server_reply; then
		notify alert "SD2NAS could not import $name (HTTP $CODE). Card left mounted. Check SD2NAS."
	elif [ "$CODE" = 200 ]; then
		if ! rc.unassigned umount "$DEVICE"; then
			[ "$LEVEL" = normal ] && LEVEL=warning
			MSG="$MSG Unmounting failed: unmount it in Unassigned Devices before removing the card."
		fi
		notify "$LEVEL" "$MSG"
	else
		notify "$LEVEL" "$MSG" # SD2NAS's own explanation; the card stays mounted
	fi
}

# deliver: pass SD2NAS's queued notices (stale staging folders, a failed
# catalog backup, files missing from the archive) to Unraid. SD2NAS hands
# each notice out once.
deliver() {
	local rc level msg
	post "/api/notifications/take?format=text" 30 ""
	rc=$?
	if [ "$rc" -ne 0 ]; then
		log "could not fetch notices from SD2NAS (curl exit $rc)"
		return
	fi
	if [ "$CODE" != 200 ]; then
		log "could not fetch notices from SD2NAS (HTTP $CODE)"
		return
	fi
	while IFS= read -r level && IFS= read -r msg; do
		case "$level" in
			normal | warning | alert) notify "$level" "$msg" ;;
			*) log "ignored a notice SD2NAS sent in an unknown form" ;;
		esac
	done <<<"$BODY"
}

case "$ACTION" in
	ADD)
		ingest
		deliver
		;;
	ERROR_MOUNT)
		notify alert "Unassigned Devices could not mount the card in $DEVICE, so it was not imported."
		;;
	NOTIFY)
		deliver
		;;
	*) # UNMOUNT, REMOVE, ERROR_UNMOUNT: nothing to do
		;;
esac
exit 0
