#!/bin/bash
# @tool: weather
# @description: Get current weather forecast from Japan Meteorological Agency (JMA) for a specified region
# @param: region string "Region name in Japanese (e.g. 東京, 大阪, 福岡, 札幌, 新潟)"
# @category: read
#
# Uses the JMA open XML feed (no API key required).
# Pair with get-location tool for automatic region detection.
# Note: region passed via env var to prevent shell injection.

INPUT=$(cat)
REGION=$(echo "$INPUT" | python3 -c "import sys,json; print(json.load(sys.stdin).get('region','東京'))" 2>/dev/null)

XML_DATA=$(curl -s "https://www.data.jma.go.jp/developer/xml/feed/regular.xml")

WEATHER_REGION="$REGION" XML_DATA="$XML_DATA" python3 <<'PYEOF' 2>/dev/null
import os, sys, json, xml.etree.ElementTree as ET
from urllib.request import urlopen

region = os.environ.get('WEATHER_REGION', '東京')
data = os.environ.get('XML_DATA', '')
if not data:
    print(json.dumps({'error': 'no data'}))
    sys.exit(0)

root = ET.fromstring(data)
ns = {'atom': 'http://www.w3.org/2005/Atom'}

alias = {
    '東京': '東京都', '大阪': '大阪府', '京都': '京都府',
    '札幌': '石狩', '名古屋': '愛知県', '横浜': '神奈川県',
    '神戸': '兵庫県', '福岡': '福岡県', '広島': '広島県',
    '仙台': '宮城県', '那覇': '沖縄本島',
}
search = alias.get(region, region)

forecast_url = None
forecast_label = None
for entry in root.findall('atom:entry', ns):
    title = entry.find('atom:title', ns)
    content = entry.find('atom:content', ns)
    link = entry.find('atom:link', ns)
    if title is None or content is None or link is None:
        continue
    t = title.text or ''
    c = content.text or ''
    if '天気予報' in t and search in c:
        forecast_url = link.get('href', '')
        forecast_label = c.strip('【】')
        break

overview_url = None
overview_label = None
for entry in root.findall('atom:entry', ns):
    title = entry.find('atom:title', ns)
    content = entry.find('atom:content', ns)
    link = entry.find('atom:link', ns)
    if title is None or content is None or link is None:
        continue
    t = title.text or ''
    c = content.text or ''
    if '天気概況' in t and search in c:
        overview_url = link.get('href', '')
        overview_label = c.strip('【】')
        break

result = {'region': region}

if forecast_url:
    try:
        xml_data = urlopen(forecast_url).read().decode('utf-8')
        froot = ET.fromstring(xml_data)
        texts = []
        for elem in froot.iter():
            tag = elem.tag.split('}')[-1] if '}' in elem.tag else elem.tag
            if tag in ('Weather', 'Sentence', 'Text') and elem.text and elem.text.strip():
                t = elem.text.strip()
                if t not in texts and len(t) > 2:
                    texts.append(t)
        result['forecast'] = {'title': forecast_label, 'details': texts[:15]}
    except Exception as e:
        result['forecast_error'] = str(e)

if overview_url:
    try:
        xml_data = urlopen(overview_url).read().decode('utf-8')
        froot = ET.fromstring(xml_data)
        texts = []
        for elem in froot.iter():
            tag = elem.tag.split('}')[-1] if '}' in elem.tag else elem.tag
            if tag in ('Sentence', 'Text') and elem.text and elem.text.strip():
                t = elem.text.strip()
                if t not in texts and len(t) > 5:
                    texts.append(t)
        result['overview'] = {'title': overview_label, 'text': texts[:5]}
    except Exception as e:
        result['overview_error'] = str(e)

if not forecast_url and not overview_url:
    regions = set()
    for entry in root.findall('atom:entry', ns):
        content = entry.find('atom:content', ns)
        title = entry.find('atom:title', ns)
        if content is not None and title is not None and '天気予報' in (title.text or ''):
            c = (content.text or '').strip('【】 ')
            if c:
                regions.add(c.replace('府県天気予報', ''))
    result['message'] = f'Region "{region}" not found'
    result['available_regions'] = sorted(regions)[:20]

print(json.dumps(result, ensure_ascii=False, indent=2))
PYEOF
