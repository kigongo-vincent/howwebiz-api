#!/bin/sh
# Production entrypoint: Redis and the API in one container.
#
# Redis is a cache only (no persistence) on loopback, so nothing outside the
# container can reach it and no password is needed. Its address is fixed
# here: whatever the env file says, prod talks to this Redis. If it ever
# exits it is restarted; meanwhile the API serves uncached.
set -eu

REDIS_PORT=6379
(
  while true; do
    redis-server \
      --bind 127.0.0.1 --port "$REDIS_PORT" --protected-mode yes \
      --save "" --appendonly no \
      --maxmemory 128mb --maxmemory-policy allkeys-lru \
      --loglevel warning
    echo "redis exited ($?), restarting in 1s" >&2
    sleep 1
  done
) &

export REDIS_ENABLED=true
export REDIS_ADDR="127.0.0.1:${REDIS_PORT}"
exec ./main
