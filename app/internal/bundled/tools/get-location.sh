#!/bin/bash
# @tool: get-location
# @description: Return the device's approximate location inferred from the system IANA timezone (NOT from GPS, IP, or network). macOS only. Output is a JSON object always containing `timezone`, `utc_offset`, `timezone_id`, `source: "system_inference"`, `accuracy: "approximate (timezone-based)"`; when the timezone matches one of the built-in entries (Asia/Tokyo, Asia/Shanghai, Asia/Seoul, America/New_York, America/Los_Angeles, America/Chicago, Europe/London, Europe/Paris, Europe/Berlin) it ALSO returns `country`, `admin_area`, `locality`, `lat`, `lon`. Any other timezone returns only the timezone fields — do NOT try to derive a city from `utc_offset` alone. If the user has set a manual override in config.json, that override is returned verbatim as `{"location": "..."}` instead. Pair with the `weather` tool by mapping `locality` → region.
# @category: read
# @timeout: 30
#
# macOS only. Uses /etc/localtime symlink to read the IANA timezone ID,
# then looks it up in a small built-in city table. Checks the agent's
# config.json `location` override first so power users can pin a value.

CONFIG_FILE="$HOME/Library/Application Support/shell-agent-v2/config.json"
if [ -f "$CONFIG_FILE" ]; then
  CACHED=$(python3 -c "
import json
with open('$CONFIG_FILE') as f:
    cfg = json.load(f)
loc = cfg.get('location', '')
if loc:
    print(json.dumps({'location': loc}, ensure_ascii=False))
" 2>/dev/null)
  if [ -n "$CACHED" ]; then
    echo "$CACHED"
    exit 0
  fi
fi

python3 -c "
import subprocess, json, time, datetime

result = {}

tz_full = time.strftime('%Z')
offset = datetime.datetime.now().astimezone().strftime('%z')
result['timezone'] = tz_full
result['utc_offset'] = offset

try:
    link = subprocess.check_output(
        ['readlink', '/etc/localtime'],
        stderr=subprocess.DEVNULL
    ).decode().strip()
    parts = link.split('zoneinfo/')
    tz_id = parts[-1] if len(parts) > 1 else link
    result['timezone_id'] = tz_id

    tz_locations = {
        'Asia/Tokyo':         {'country': 'Japan',       'admin_area': '',           'locality': 'Tokyo',       'lat': 35.68, 'lon': 139.77},
        'Asia/Shanghai':      {'country': 'China',       'admin_area': '',           'locality': 'Shanghai',    'lat': 31.23, 'lon': 121.47},
        'Asia/Seoul':         {'country': 'South Korea', 'admin_area': '',           'locality': 'Seoul',       'lat': 37.57, 'lon': 126.98},
        'America/New_York':   {'country': 'USA',         'admin_area': 'New York',   'locality': 'New York',    'lat': 40.71, 'lon': -74.01},
        'America/Los_Angeles':{'country': 'USA',         'admin_area': 'California', 'locality': 'Los Angeles', 'lat': 34.05, 'lon': -118.24},
        'America/Chicago':    {'country': 'USA',         'admin_area': 'Illinois',   'locality': 'Chicago',     'lat': 41.88, 'lon': -87.63},
        'Europe/London':      {'country': 'UK',          'admin_area': '',           'locality': 'London',      'lat': 51.51, 'lon': -0.13},
        'Europe/Paris':       {'country': 'France',      'admin_area': '',           'locality': 'Paris',       'lat': 48.86, 'lon': 2.35},
        'Europe/Berlin':      {'country': 'Germany',     'admin_area': '',           'locality': 'Berlin',      'lat': 52.52, 'lon': 13.41},
    }
    if tz_id in tz_locations:
        result.update(tz_locations[tz_id])
except:
    pass

result['source'] = 'system_inference'
result['accuracy'] = 'approximate (timezone-based)'
print(json.dumps(result, ensure_ascii=False, indent=2))
" 2>/dev/null
