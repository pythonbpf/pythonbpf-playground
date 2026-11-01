#!/usr/bin/env bash
# One-time: publish the playground through the host's existing Cloudflare tunnel.
# Run on the host as the user that owns ~/.cloudflared/cert.pem (not root).
#   HOSTNAME_=pythonbpf.xeon.me TUNNEL=67aaaf69-fb81-47d9-bd4c-2acfa9dc1a7b ./cloudflared-route.sh
set -euo pipefail
HOSTNAME_="${HOSTNAME_:-pythonbpf.xeon.me}"
TUNNEL="${TUNNEL:-67aaaf69-fb81-47d9-bd4c-2acfa9dc1a7b}"
CFG=/etc/cloudflared/config.yml

if ! sudo grep -q "hostname: ${HOSTNAME_}\$" "$CFG"; then
  sudo cp "$CFG" "$CFG.bak.$(date +%Y%m%d%H%M%S)"
  # Insert above the catch-all 404 rule (order matters).
  sudo sed -i "s|^  - service: http_status:404\$|  - hostname: ${HOSTNAME_}\n    service: http://127.0.0.1:8090\n  - service: http_status:404|" "$CFG"
fi
sudo cloudflared tunnel --config "$CFG" ingress validate
sudo cloudflared tunnel --config "$CFG" ingress rule "https://${HOSTNAME_}"
cloudflared tunnel route dns "$TUNNEL" "$HOSTNAME_"   # CNAME -> <tunnel>.cfargotunnel.com
sudo systemctl restart cloudflared
sleep 3
curl -fsS "https://${HOSTNAME_}/healthz" && echo
