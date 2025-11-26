#!/bin/bash

# Test script to query Traefik API from within Docker network

echo "Testing Traefik API..."
echo "====================="
echo ""

# Option 1: Using docker exec if traefik container is running
echo "1. Querying from traefik container itself:"
docker exec traefik wget -qO- http://localhost:8080/api/http/routers | jq '.'

echo ""
echo "2. Getting just router names and rules:"
docker exec traefik wget -qO- http://localhost:8080/api/http/routers | \
  jq -r 'to_entries[] | "\(.key): \(.value.rule)"'

echo ""
echo "3. Getting only enabled routers:"
docker exec traefik wget -qO- http://localhost:8080/api/http/routers | \
  jq 'to_entries[] | select(.value.status == "enabled") | {name: .key, rule: .value.rule}'
