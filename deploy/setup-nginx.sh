#!/usr/bin/env bash
# Sets up nginx vhosts + TLS for RITEL35K on the Dokploy host. Run once as root:  sudo bash setup-nginx.sh
# Idempotent: re-running rewrites the two vhost files; nginx is only reloaded after `nginx -t` passes, otherwise the
# previous files are restored. Existing vhosts of other apps are never touched.
set -euo pipefail

WEB_HOST=ritel35k.divine.co.id
API_HOST=ritel35k-be.divine.co.id
WEB_PORT=3130
API_PORT=3131
EMAIL="${CERTBOT_EMAIL:-nicholas@divine.co.id}"
AVAIL=/etc/nginx/sites-available
ENAB=/etc/nginx/sites-enabled

[ "$(id -u)" = 0 ] || { echo "run as root (sudo bash $0)"; exit 1; }
command -v nginx >/dev/null || { echo "nginx not found"; exit 1; }

backup() { [ -f "$1" ] && cp -a "$1" "$1.bak.$(date +%s)" || true; }

write_vhost() { # name host port extra
  local name=$1 host=$2 port=$3 extra=$4
  backup "$AVAIL/$name"
  cat > "$AVAIL/$name" <<CONF
server {
    listen 80;
    listen [::]:80;
    server_name $host;

    location / {
        proxy_pass http://127.0.0.1:$port;
        proxy_http_version 1.1;
        proxy_set_header Host \$host;
        proxy_set_header X-Real-IP \$remote_addr;
        proxy_set_header X-Forwarded-For \$proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto \$scheme;
        proxy_set_header X-Forwarded-Host \$host;
$extra
    }
}
CONF
  ln -sf "$AVAIL/$name" "$ENAB/$name"
}

# frontend: live dashboard uses SSE, so no buffering and a long read timeout
write_vhost ritel35k-web "$WEB_HOST" "$WEB_PORT" "        proxy_buffering off;
        proxy_read_timeout 1h;
        client_max_body_size 20m;"
# API: also serves the store agents (/api/v1/edge/*), which can upload sizeable offline batches
write_vhost ritel35k-api "$API_HOST" "$API_PORT" "        client_max_body_size 20m;
        proxy_read_timeout 120s;"

if ! nginx -t; then
  echo "nginx config test failed: rolling back" >&2
  rm -f "$ENAB/ritel35k-web" "$ENAB/ritel35k-api"
  exit 1
fi
systemctl reload nginx
echo "http vhosts live; requesting certificates…"

command -v certbot >/dev/null || { apt-get update -y && apt-get install -y certbot python3-certbot-nginx; }
certbot --nginx --non-interactive --agree-tos -m "$EMAIL" --redirect -d "$WEB_HOST" -d "$API_HOST" || \
  { echo "certbot failed (DNS must point to this server; ports 80/443 open). The http vhosts stay active." >&2; exit 1; }

nginx -t && systemctl reload nginx
echo "done:"
curl -s -m 10 "https://$WEB_HOST/ping" || true; echo
curl -s -m 10 "https://$API_HOST/ready" || true; echo
