#!/bin/bash
# adaptive_cc_hold_when_idle isolated check: builds its own openresty + mocks, cleans up after itself.
# With the switch on, low or absent traffic never lowers cc: no slack shrink and cc never expires.
# Growth under pressure and overload shrink are unchanged. The ctl route (switch off) runs the same
# load side by side, so every "held" assertion has a control that does move.
#   ① /_tps_status reports the switch (on / off / invalid string → off, logged)
#   ② fresh pool still starts at min (hold does not change the start point)
#   ③ high load raises cc on both routes (hold does not block growth)
#   ④ light load: hold keeps its peak, ctl shrinks
#   ⑤ full idle past both TTLs: hold keeps its value, ctl expires
#   ⑥ overload still shrinks the held route
set -u
HERE="$(cd "$(dirname "$0")" && pwd)"
ENGINE="${ENGINE:-$HERE/../session_base.conf}"; MOCK="${MOCK:-$HERE/mock_vllm_sse.py}"
OPENRESTY="${OPENRESTY:-/usr/local/openresty/bin/openresty}"; PY="${PY:-python3}"
PREFIX="${PREFIX:-/tmp/routeopts}"; KEY="${API_KEY:-}"
MPORTS="28991 28992"
cleanup(){ "$OPENRESTY" -p "$PREFIX" -c "$PREFIX/nginx.conf" -s stop 2>/dev/null; sleep 1
  for pid in $(ps -eo pid,cmd|grep "$PREFIX/nginx"|grep -v grep|awk '{print $1}'); do kill -9 "$pid" 2>/dev/null; done
  for p in $MPORTS; do for pid in $(ps -eo pid,cmd|grep "mock_vllm_sse.py --port $p"|grep -v grep|awk '{print $1}'); do kill -9 "$pid" 2>/dev/null; done; done
  rm -rf "$PREFIX"; }
trap cleanup EXIT
mkdir -p "$PREFIX/logs" "$PREFIX/temp"
start=$(grep -n "^init_by_lua_block {" "$ENGINE"|head -1|cut -d: -f1)
srv=$(grep -n "^server {" "$ENGINE"|head -1|cut -d: -f1); end=$(awk -v s="$srv" 'NR<s && /^}/{l=NR} END{print l}' "$ENGINE")
sed -n "${start},${end}p" "$ENGINE" > "$PREFIX/initblock.conf"
source "$HERE/lib_initblock.sh"; prepend_lua_path "$PREFIX/initblock.conf" "$HERE/../lua"

ROUTES="plain strs junk"
{
cat <<'EOF'
worker_processes 1; error_log logs/error.log warn; pid logs/nginx.pid;
events { worker_connections 2048; }
http {
  access_log off; client_body_temp_path temp/cb; proxy_temp_path temp/p; fastcgi_temp_path temp/f; uwsgi_temp_path temp/u; scgi_temp_path temp/s;
  lua_shared_dict ttft_stat 256k; lua_shared_dict tps_stat 256k; lua_shared_dict api_keys 1m; lua_shared_dict reject_stat 128k;
EOF
for r in $ROUTES; do
  echo "  lua_shared_dict active_conns_$r 4m; lua_shared_dict cluster_avg_$r 16k; lua_shared_dict lc_locks_$r 1m; lua_shared_dict bad_peers_$r 1m; lua_shared_dict bodylog_ctl_$r 1m; lua_shared_dict cch_ctl_$r 1m;"
done
cat <<'EOF'
  upstream vllm_backends { server 0.0.0.1:1; balancer_by_lua_block { _G.do_balancer() } }
  include initblock.conf;
  init_worker_by_lua_block {
    math.randomseed(ngx.now()*1000 + ngx.worker.pid())
    _G.TPS_BUCKETS = {5,10,20,40,80,150,300}
    -- No adaptive_cc globals pinned here: ① compares the catalog against the real engine defaults.
    local peers = {{"127.0.0.1",28991,"m1",0,100},{"127.0.0.1",28992,"m2",0,100}}   -- static max 200
    local dicts = {bodylog_default_enabled=false}   -- bodylog is not in the catalog; keep the mocks quiet
    local function R(x) local t={peers=peers} for k,v in pairs(dicts) do t[k]=v end for k,v in pairs(x) do t[k]=v end return t end
    _G.register_route("plain", function() return R({}) end)   -- sets nothing tunable: engine defaults throughout
    _G.register_route("strs",  function() return R({cross_tier_fallback="False", adaptive_cc_hold_when_idle="no",
                                                     adaptive_cc_init_frac=0.1, adaptive_cc_min=80, max_more_tries=3}) end)
    _G.register_route("junk",  function() return R({cross_tier_fallback="maybe", adaptive_cc_hold_when_idle="maybe",
                                                     adaptive_cc_init=500}) end)
  }
EOF
port=19760
for r in $ROUTES; do
  echo "  server { listen $port; server_name _; set \$routed_session_id \"-\"; set \$routed_source \"-\"; set \$routed_mode \"-\"; set \$routed_peer \"-\"; set \$routed_dict \"\";"
  echo "    location /v1/ { access_by_lua_block { _G.do_route(_G.__route_opts.$r) } body_filter_by_lua_block { _G.bodylog_filter_chunk() } proxy_pass http://vllm_backends; proxy_http_version 1.1; proxy_buffering off; proxy_read_timeout 3600s; log_by_lua_block { _G.do_log_release(_G.__route_opts.$r) } }"
  echo "    location = /_route_opts { content_by_lua_block { _G.dbg_route_opts(_G.__route_opts.$r) } } }"
  port=$((port+1))
done
echo "}"
} > "$PREFIX/nginx.conf"

"$OPENRESTY" -p "$PREFIX" -c "$PREFIX/nginx.conf" -t 2>&1 | tail -1 | grep -q "successful" || { echo "nginx -t FAILED"; "$OPENRESTY" -p "$PREFIX" -c "$PREFIX/nginx.conf" -t; exit 1; }
for p in $MPORTS; do setsid nohup "$PY" "$MOCK" --port $p --name m-$p --output-len 100 --chunk-delay-ms 12 --prefill-delay-ms 15 >"$PREFIX/mock_$p.log" 2>&1 & disown; done
sleep 3; "$OPENRESTY" -p "$PREFIX" -c "$PREFIX/nginx.conf"; sleep 2

P=0; F=0; ok(){ echo "  ✓ $1"; P=$((P+1)); }; no(){ echo "  ✗ FAIL: $1"; F=$((F+1)); }
H="Content-Type: application/json"; A="Authorization: Bearer $KEY"
PLAIN=http://127.0.0.1:19760; STRS=http://127.0.0.1:19761; JUNK=http://127.0.0.1:19762
body(){ echo "{\"stream_options\":{\"include_usage\":true},\"model\":\"x\",\"stream\":true,\"chunk_delay_ms\":$1,\"max_tokens\":$2,\"prefill_delay_ms\":15,\"messages\":[{\"role\":\"user\",\"content\":\"hi\"}]}"; }
fire(){ curl -s -o /dev/null -N -H "$A" -H "$H" -d "$(body $2 $3)" "$1/v1/chat/completions"; }
# keep ~n overlapping requests for dur seconds (chunk ms x tokens per request)
load_n(){ local u=$1 n=$2 dur=$3 ck=${4:-12} tk=${5:-100}; local endt=$((SECONDS+dur))
  while [ "$SECONDS" -lt "$endt" ]; do for i in $(seq 1 $n); do fire "$u" $ck $tk >/dev/null 2>&1 & done; sleep 0.8; done; wait; }
# fire n simultaneous long requests (12ms x 300 = ~3.6s each) and print how many got 200
burst_ok(){ local u=$1 n=$2 d="$PREFIX/burst_$RANDOM"; mkdir -p "$d"
  for i in $(seq 1 $n); do ( curl -s -o /dev/null -N -w '%{http_code}\n' -H "$A" -H "$H" -d "$(body 12 300)" "$u/v1/chat/completions" > "$d/$i" ) & done
  wait; cat "$d"/* | grep -c '^200$'; }
fv(){ curl -s "$1/_tps_status" | python3 -c "import sys,json;d=json.load(sys.stdin);v=d.get('$2');print(v.get('_') if isinstance(v,dict) else v)"; }
num(){ [ "$1" = "None" ] && echo -1 || echo "$1"; }

# ro <url> <python expr over d (decoded /_route_opts) and D (directives by key)>
ro(){ curl -s "$1/_route_opts" | python3 -c "import sys,json;d=json.load(sys.stdin);D={x['key']:x for x in d['directives']};r=($2);print(r if isinstance(r,tuple) else r)"; }
echo "########## /_route_opts ##########"
# ① literal catalog defaults == effective values on a route that sets nothing
drift=$(curl -s "$PLAIN/_route_opts" | python3 -c '
import sys, json
d = json.load(sys.stdin); bad = []; checked = 0
for x in d["directives"]:
    f, v = x["fallback"], x["value"]
    if f in ("on", "off"): want = (f == "on")
    elif f.startswith("/"): want = f
    else:
        try: want = float(f)
        except ValueError: continue          # derived default ("the floor", "the TPS window" ...)
        v = None if v is None else float(v)
    checked += 1
    if v != want or x["set"]: bad.append("%s: fallback=%s value=%s set=%s" % (x["key"], f, x["value"], x["set"]))
print(checked, "|", "; ".join(bad))')
n=${drift%% |*}; rest=${drift#*| }
[ "$n" -ge 25 ] && [ -z "$rest" ] && ok "① $n literal catalog defaults match the engine" || no "① checked=$n drift: $rest"
[ "$(ro $PLAIN 'len(d["directives"])')" -ge 38 ] && ok "① catalog served ($(ro $PLAIN 'len(d["directives"])') directives, console shape)" || no "① directives missing"
[ "$(ro $PLAIN 'sorted(set(k for x in d["directives"] for k in x))')" = "['detail', 'fallback', 'group', 'key', 'kind', 'label', 'needsAdaptive', 'set', 'value']" ] && ok "① item fields match the console Directive (+value, set)" || no "① fields: $(ro $PLAIN 'sorted(set(k for x in d["directives"] for k in x))')"

# ② new defaults
[ "$(ro $PLAIN 'D["adaptive_cc_hold_when_idle"]["value"], D["adaptive_cc_hold_when_idle"]["set"]')" = "(True, False)" ] && ok "② hold_when_idle defaults to on (not set by the route)" || no "② hold: $(ro $PLAIN 'D["adaptive_cc_hold_when_idle"]')"
[ "$(ro $PLAIN 'D["cross_tier_fallback"]["value"]')" = "False" ] && ok "② cross_tier_fallback defaults to off" || no "② cross_tier: $(ro $PLAIN 'D["cross_tier_fallback"]["value"]')"
[ "$(ro $PLAIN 'D["adaptive_cc_init"]["value"] is None and d["adaptive_cc_derived"]["_"] == {"static_max": 200, "min": 80, "init": 80}')" = "True" ] && ok "② no init: derived init == min (80 of 200)" || no "② derived: $(ro $PLAIN 'd["adaptive_cc_derived"]')"

# ③ boolean parsing
[ "$(ro $STRS 'D["cross_tier_fallback"]["value"], D["adaptive_cc_hold_when_idle"]["value"]')" = "(False, False)" ] && ok "③ \"False\" and \"no\" read as false" || no "③ strs: $(ro $STRS 'D["cross_tier_fallback"]["value"], D["adaptive_cc_hold_when_idle"]["value"]')"
grep -q 'cross_tier_fallback=False (string) read as false' "$PREFIX/logs/error.log" && ok "③ WARN for the non-boolean \"False\"" || no "③ no WARN for \"False\""
[ "$(ro $JUNK 'D["cross_tier_fallback"]["value"], D["adaptive_cc_hold_when_idle"]["value"]')" = "(False, True)" ] && ok "③ \"maybe\" -> defaults (cross_tier off, hold on)" || no "③ junk: $(ro $JUNK 'D["cross_tier_fallback"]["value"], D["adaptive_cc_hold_when_idle"]["value"]')"
[ "$(grep -c 'is not a boolean - using the default' "$PREFIX/logs/error.log")" -ge 2 ] && ok "③ ERR for both unrecognised values" || no "③ ERR lines: $(grep -c 'is not a boolean' "$PREFIX/logs/error.log")"

# ④ init outside [min, max]
grep -q '\[strs\] adaptive_cc_init_frac=0.1 gives 20, outside \[min 80, max 200\] - effective init is 80' "$PREFIX/logs/error.log" && ok "④ WARN: init_frac below the floor" || no "④ no WARN for init_frac below min"
grep -q '\[junk\] adaptive_cc_init=500 gives 500, outside \[min 80, max 200\] - effective init is 200' "$PREFIX/logs/error.log" && ok "④ WARN: init above the maximum" || no "④ no WARN for init above max"
[ "$(grep -c 'outside \[min' "$PREFIX/logs/error.log")" = "2" ] && ok "④ logged once per route (2 lines, not per request)" || no "④ WARN count $(grep -c 'outside \[min' "$PREFIX/logs/error.log")"
[ "$(ro $JUNK 'd["adaptive_cc_derived"]["_"]["init"]')" = "200" ] && ok "④ derived init clamped to max (200)" || no "④ junk init $(ro $JUNK 'd["adaptive_cc_derived"]')"

# ⑤ set flags, redaction, peers
[ "$(ro $STRS 'D["max_more_tries"]["value"], D["max_more_tries"]["set"], D["tps_window"]["set"]')" = "(3, True, False)" ] && ok "⑤ set flags (max_more_tries set, tps_window default)" || no "⑤ set: $(ro $STRS 'D["max_more_tries"], D["tps_window"]["set"]')"
[ "$(ro $PLAIN 'd["other_options"].get("api_keys_dict")')" = "<redacted>" ] && ok "⑤ secret-looking option redacted" || no "⑤ api_keys_dict: $(ro $PLAIN 'd["other_options"].get("api_keys_dict")')"
[ "$(ro $PLAIN 'd["peers"], "peers" in d["other_options"]')" = "({'count': 2}, False)" ] && ok "⑤ peers reduced to a count" || no "⑤ peers: $(ro $PLAIN 'd["peers"]')"

echo "==== result: PASS=$P FAIL=$F ===="
[ "$F" -eq 0 ] && echo "ALL GOOD" || echo "HAS FAILURES"
