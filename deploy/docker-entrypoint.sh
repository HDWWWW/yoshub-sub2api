#!/bin/sh
set -e

# Fix data directory permissions when running as root.
# Docker named volumes / host bind-mounts may be owned by root,
# preventing the non-root sub2api user from writing files.
if [ "$(id -u)" = "0" ]; then
    mkdir -p /app/data
    # Use || true to avoid failure on read-only mounted files (e.g. config.yaml:ro)
    chown -R sub2api:sub2api /app/data 2>/dev/null || true
    # Re-invoke this script as sub2api so the flag-detection below
    # also runs under the correct user.
    exec su-exec sub2api "$0" "$@"
fi

# Compatibility: if the first arg looks like a flag (e.g. --help),
# prepend the default binary so it behaves the same as the old
# ENTRYPOINT ["/app/sub2api"] style.
if [ "${1#-}" != "$1" ]; then
    set -- /app/sub2api "$@"
fi

# An update is scoped to the baked binary. Recreating the same image retains it,
# while explicitly pulling a different image starts that image's own program.
if [ "${1:-}" = /app/sub2api ] && [ "${SUB2API_DISABLE_SELF_UPDATE:-0}" != 1 ]; then
    if [ -s /app/sub2api.image-id ]; then
        IFS= read -r image_id < /app/sub2api.image-id
    else
        # Also support custom images that copy this entrypoint without the marker.
        image_id=$(sha256sum /app/sub2api)
        image_id=${image_id%% *}
    fi
    SUB2API_UPDATE_DIR="/app/data/.yoshub-updates/$image_id"
    export SUB2API_UPDATE_DIR
    if [ -x "$SUB2API_UPDATE_DIR/sub2api" ]; then
        shift
        set -- "$SUB2API_UPDATE_DIR/sub2api" "$@"
    fi
    if ! (umask 077 && mkdir -p "$SUB2API_UPDATE_DIR") || [ ! -w "$SUB2API_UPDATE_DIR" ]; then
        export SUB2API_DISABLE_SELF_UPDATE=1
    fi
fi

exec "$@"
