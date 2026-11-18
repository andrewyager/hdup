#!/bin/bash
# Run hdup until all files are done. Ctrl-C stops hdup and this loop.
# Use: ./run-loop.sh [parts-per-deck]
# Put this script in the same directory as hdup and job.toml.
cd "$(dirname "$0")" || exit 1
parts="${1:-8}"
while true; do
  ./hdup upload --yes --parts-per-deck "$parts" job.toml 2>&1 | tee -a hdup-run.log
  rc=${PIPESTATUS[0]}
  if [ "$rc" -eq 0 ] || [ "$rc" -eq 130 ]; then
    echo "$(date) hdup exit code $rc; stop" | tee -a hdup-run.log
    break
  fi
  echo "$(date) hdup exit code $rc; next try in 60 s" | tee -a hdup-run.log
  sleep 60
done
