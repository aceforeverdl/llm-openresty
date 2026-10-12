#!/usr/bin/env bash
# Per-route request body limit, on the real session_base.conf (dispatch + http-level defaults), the real
# router_locations.inc and lua, with two routes behind the dispatch socket:
#   dflt -- sets nothing: inherits the http-level 10m
#   big  -- sets `client_max_body_size 50m;` in its own server block, as autoconfig renders it
# Checks: nginx -t accepts the override (the shared include no longer sets the directive, so it is not a
# duplicate); 5MB to dflt = 200; 12MB to dflt = 413; 12MB to big = 200 (override above the default works);
# 120MB to big = 413 at dispatch (100m is the ceiling). Runs where openresty is installed (the test box).
set -u
HERE="$(cd "$(dirname "$0")" && pwd)"; BASE="$HERE/.."
OPENRESTY="${OPENRESTY:-/usr/local/openresty/bin/openresty}"; PY="${PY:-python3}"
MOCK="${MOCK:-$HERE/mock_vllm_sse.py}"; PREFIX="${PREFIX:-/tmp/bodysize}"; KEY="${API_KEY:-}"
cleanup(){ "$OPENRESTY" -p "$PREFIX" -c nginx.conf -s stop 2>/dev/null; sleep 1; pkill -9 -f "$PREFIX/nginx" 2>/dev/null
  for p in 28955 28956; do pkill -9 -f "mock_vllm_sse.py --port $p" 2>/dev/null; done; }
trap cleanup EXIT
rm -rf "$PREFIX"; mkdir -p "$PREFIX/logs" "$PREFIX/temp" "$PREFIX/sock" "$PREFIX/conf.d/routes" "$PREFIX/conf.d/lua"

# The real base, router_locations.inc and lua, in the image layout (conf.d/*.conf + conf.d/routes/*.conf + conf.d/lua/).
# `listen unix:` needs an absolute path: point session_base's fixed sock directory at this harness's prefix/sock.
cp "$BASE/session_base.conf" "$PREFIX/conf.d/session_base.conf"
sed -i "s#/usr/local/openresty/nginx/sock#$PREFIX/sock#g" "$PREFIX/conf.d/session_base.conf"
# A live openresty on the test box may hold 8080/8090: move dispatch to 19780 and health to 19790.
sed -i "s#listen 8080;#listen 19780;#; s#listen 8090;#listen 19790;#" "$PREFIX/conf.d/session_base.conf"
cp "$BASE/router_locations.inc" "$PREFIX/conf.d/router_locations.inc"
cp "$BASE/lua/"*.lua "$PREFIX/conf.d/lua/"
cp -r "$BASE/vendor" "$PREFIX/vendor" 2>/dev/null

# Per-route confs in the form autoconfig renders: a unix socket behind dispatch, plus the route's own directives.
mkroute(){ local key="$1" mport="$2" extra="$3"
cat > "$PREFIX/conf.d/routes/session_route_$key.conf" <<EOF
lua_shared_dict active_conns_$key 4m; lua_shared_dict cluster_avg_$key 16k; lua_shared_dict lc_locks_$key 1m;
lua_shared_dict bad_peers_$key 1m; lua_shared_dict bodylog_ctl_$key 1m; lua_shared_dict cch_ctl_$key 1m;
server {
    listen unix:$PREFIX/sock/$key.sock;
    server_name _;
    set \$route "$key";
    $extra
    set_by_lua_block \$__${key}_init { _G.register_route("$key", function() return {
        peers={{"127.0.0.1",$mport,"m-$key"}}, default_max=50, bodylog_default_enabled=false,
        health_check_interval=9999, adaptive_cc=false } end) return "" }
    include conf.d/router_locations.inc;
}
EOF
}
mkroute dflt 28955 ""
mkroute big  28956 "client_max_body_size 50m;"

cat > "$PREFIX/nginx.conf" <<EOF
worker_processes 1; error_log logs/error.log warn; pid logs/nginx.pid;
env BODYLOG_LISTENER_HOST=127.0.0.1; env BODYLOG_LISTENER_PORT=59999;
events { worker_connections 2048; }
http {
  access_log off; client_body_temp_path temp/cb; proxy_temp_path temp/p; fastcgi_temp_path temp/f; uwsgi_temp_path temp/u; scgi_temp_path temp/s;
  log_format session_log escape=json '{"status":\$status,"sid":"\$routed_session_id","peer":"\$routed_peer"}';   # referenced by router_locations.inc (content not tested)
  lua_package_path '$PREFIX/conf.d/lua/?.lua;$PREFIX/vendor/?.lua;;';
  include conf.d/*.conf;          # the real session_base.conf (dicts, upstream, init, dispatch, health)
  include conf.d/routes/*.conf;   # per-route servers (unix sockets)
}
EOF

echo "=== nginx -t ==="; "$OPENRESTY" -p "$PREFIX" -c nginx.conf -t 2>&1 | tail -2 | sed 's/^/  /'
"$OPENRESTY" -p "$PREFIX" -c nginx.conf -t 2>&1 | tail -1 | grep -q successful || { echo "  --- error ---"; "$OPENRESTY" -p "$PREFIX" -c nginx.conf -t 2>&1 | tail -8; exit 1; }
for p in 28955 28956; do setsid nohup "$PY" "$MOCK" --port $p --name m-$p --output-len 20 --chunk-delay-ms 10 --prefill-delay-ms 20 >"$PREFIX/mock_$p.log" 2>&1 & disown; done
sleep 3; "$OPENRESTY" -p "$PREFIX" -c nginx.conf; sleep 2

P=0; F=0; ok(){ echo "  PASS: $1"; P=$((P+1)); }; no(){ echo "  FAIL: $1"; F=$((F+1)); }
mkbody(){ "$PY" -c "import json,sys;print(json.dumps({'model':'x','stream':False,'max_tokens':4,'messages':[{'role':'user','content':'a'*int(sys.argv[1])}]}))" "$1" > "$PREFIX/body_$1"; }
code(){ curl -s -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $KEY" -H "Content-Type: application/json" \
          --data-binary @"$PREFIX/body_$2" "http://127.0.0.1:19780/$1/v1/chat/completions"; }
for n in 5000000 12000000 120000000; do mkbody $n; done
echo "=== per-route body limit ==="
c=$(code dflt 5000000);   [ "$c" = 200 ] && ok "5MB to the default route -> 200" || no "5MB dflt -> $c"
c=$(code dflt 12000000);  [ "$c" = 413 ] && ok "12MB to the default route -> 413 (10m default)" || no "12MB dflt -> $c"
c=$(code big 12000000);   [ "$c" = 200 ] && ok "12MB to the 50m route -> 200 (override above the default)" || no "12MB big -> $c"
c=$(code big 120000000);  [ "$c" = 413 ] && ok "120MB to the 50m route -> 413 (dispatch 100m ceiling)" || no "120MB big -> $c"
grep -q 'client intended to send too large body: 12000' "$PREFIX/logs/error.log" && ok "the 12MB refusal is logged" || no "no too-large log line for 12MB"
echo "==== result: PASS=$P FAIL=$F ===="
[ "$F" -eq 0 ] && echo "ALL GOOD" || { echo "HAS FAILURES"; exit 1; }
