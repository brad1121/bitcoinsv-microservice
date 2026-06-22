#!/bin/sh
set -eu

mine_interval="${MINE_INTERVAL_SECONDS:-60}"
initial_height="${INITIAL_BLOCK_HEIGHT:-10001}"

/entrypoint.sh bitcoind \
  -regtest \
  -server \
  -listen=1 \
  -txindex=1 \
  -port=18444 \
  -bind=0.0.0.0:18444 \
  -rpcbind=0.0.0.0 \
  -rpcport=18443 \
  -rpcallowip=0.0.0.0/0 \
  -rpcuser=bsv \
  -rpcpassword=bsv \
  -minminingtxfee=0.00000001 \
  -printtoconsole &
node_pid="$!"

mine_loop() {
  until bitcoin-cli -regtest -rpcport=18443 -rpcuser=bsv -rpcpassword=bsv getblockchaininfo >/dev/null 2>&1; do
    sleep 1
  done
  height="$(bitcoin-cli -regtest -rpcport=18443 -rpcuser=bsv -rpcpassword=bsv getblockcount 2>/dev/null || echo 0)"
  if [ "$height" -lt "$initial_height" ]; then
    bitcoin-cli -regtest -rpcport=18443 -rpcuser=bsv -rpcpassword=bsv generate "$((initial_height - height))" >/dev/null 2>&1 || true
  fi
  while kill -0 "$node_pid" >/dev/null 2>&1; do
    sleep "$mine_interval"
    bitcoin-cli -regtest -rpcport=18443 -rpcuser=bsv -rpcpassword=bsv generate 1 >/dev/null 2>&1 || true
  done
}

mine_loop &
miner_pid="$!"

trap 'kill -TERM "$node_pid" "$miner_pid" 2>/dev/null || true' INT TERM
wait "$node_pid"
status="$?"
kill -TERM "$miner_pid" 2>/dev/null || true
wait "$miner_pid" 2>/dev/null || true
exit "$status"
