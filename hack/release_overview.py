#!/usr/bin/env python3
import argparse
import sys
import requests
from datetime import datetime, date
from dateutil.parser import ParserError, parse
import re
import colorsys
from typing import List, Dict

## This is a simple tool for determining if a new z-stream release should be created for each version of the WMCO. 
## This tool supplies information about: 
## - The support phase of the OCP version the WMCO release is associated with
## - The end of life date for each version 
## - The latest z-stream version of WMCO 
## - The date of the last z-stream release 
## - The grade of the last z-stream release 

## Usage
## The CLI can be run by using ./release-overview.py

REPO_ID = "6446a42c036802e6e3d04de0"
PYXIS_API = "https://catalog.redhat.com/api/containers/v1"
PRODUCT_LIFECYCLE_API = "https://access.redhat.com/product-life-cycles/api/v1/products"
HTTP_TIMEOUT_SECONDS = 15

def to_date(value):
    if not value or value == "N/A":
        return None
    try:
        return parse(value).date()
    except (ParserError, TypeError, ValueError):
        return None

def get_eol_date(version):
    latest = None

    for phase in version["phases"]:
        end = to_date(phase.get("end_date"))
        if end:
            if not latest or end > latest:
                latest = end

    return latest

def get_current_phase(version, today=None):
    today = today or date.today()

    active = None

    for phase in version["phases"]:
        start_raw = phase.get("start_date")
        end_raw = phase.get("end_date")

        start = to_date(start_raw)
        end = to_date(end_raw)
        
        if not start and not end:
            continue

        if isinstance(start_raw, str) and start_raw.strip().startswith("GA of"):
            if end and today <= end:
                return "Full Support"
            continue
        
        if start and end:
            if start <= today <= end:
                return phase["name"]

        if not start and end:
            if today <= end:
                active = phase["name"]

    return active

def summarize_versions():
    api_data = get_lifecycle_data()
    today = date.today()
    product = api_data["data"][0]
    results = []
    images_data = get_grade_data()
    grade_data = extract_grade_data(images_data.get('data', images_data))

    for version in product["versions"]:
        current_phase = get_current_phase(version, today)
        eol_date = get_eol_date(version)
        wmco_z_stream_version = "unknown"
        latest_grade = "unknown"
        try:
            minor_version = version["name"].split('.')[1]
        except:
            continue

        try:
            grade_info_list = grade_data[f"10.{minor_version}"]
            most_recent = grade_info_list[0]  # First element is most recent z-stream
            
        
            wmco_z_stream_version =  most_recent['version']

            grades = most_recent.get('freshness_grades', [])
            if grades:
                latest_grade = grades[0].get('grade') if isinstance(grades[0], dict) else grades[0]
            
            repos = most_recent.get("repositories", [])
            if repos:
                creation_date = datetime.fromisoformat(repos[0].get("push_date")).date()
            
        except:
            continue

        results.append({
            "version": version["name"],
            "current_phase": current_phase,
            "creation_date": creation_date, 
            "end_of_life": eol_date,
            "wmco_z_stream_version": wmco_z_stream_version,
            "latest_grade": latest_grade 
        })

    return results

def get_lifecycle_data():
    params = {
        "name": "Openshift Container Platform"
    }
    headers = {
        "User-Agent": "Mozilla/5.0",
        "Accept": "application/json"
    }

    res = requests.get(PRODUCT_LIFECYCLE_API, headers=headers, params=params, timeout=HTTP_TIMEOUT_SECONDS)
    res.raise_for_status()

    data = res.json()
    return data


def get_grade_data():
    session = requests.Session()

    url = f"{PYXIS_API}/repositories/id/{REPO_ID}"
    resp = session.get(url, timeout=HTTP_TIMEOUT_SECONDS)
    resp.raise_for_status()
    repo_data = resp.json()

    if '_links' in repo_data and 'images' in repo_data['_links']:
        images_path = repo_data['_links']['images']['href']
        images_url = f"https://catalog.redhat.com/api/containers{images_path}"
    else:
        # Fallback to constructing from registry/repository
        registry = repo_data.get('registry', 'registry.access.redhat.com')
        repository = repo_data.get('repository', '')
        images_url = f"{PYXIS_API}/repositories/registry/{registry}/repository/{repository}/images"

    resp = session.get(images_url, timeout=HTTP_TIMEOUT_SECONDS)
    resp.raise_for_status()

    return resp.json()

def extract_grade_data(images_data: List[Dict]) -> Dict[str, List[Dict]]:
    grade_data = {}
    version_pattern = re.compile(r'^v(\d+\.\d+\.\d+)$')

    for image in images_data:
        tags = []
        repositories = image.get('repositories', [])
        for repo in repositories:
            tags.extend(repo.get('tags', []))

        full_version = None
        for tag in tags:
            tag_name = tag.get('name') if isinstance(tag, dict) else tag
            match = version_pattern.match(tag_name)
            if match:
                full_version = match.group(1)
                break

        if not full_version:
            continue

        parts = full_version.split('.')
        minor_version = f"{parts[0]}.{parts[1]}"

        grade_info = {
            '_id': image.get('_id'),
            'docker_image_digest': image.get('docker_image_digest'),
            'version': full_version,
            'tags': [t.get('name') if isinstance(t, dict) else t for t in tags],
            'freshness_grades': image.get('freshness_grades', []),
            'repositories': image.get('repositories', []),
            'parsed_data': {
                'architecture': image.get('parsed_data', {}).get('architecture'),
                'layers': len(image.get('parsed_data', {}).get('layers', [])),
            },
            'sum_layer_size_bytes': image.get('sum_layer_size_bytes'),
            'vulnerabilities': image.get('vulnerabilities', {}),
        }

        if minor_version not in grade_data:
            grade_data[minor_version] = []
        grade_data[minor_version].append(grade_info)

    for minor_version in grade_data:
        grade_data[minor_version].sort(
            key=lambda x: tuple(map(int, x['version'].split('.'))),
            reverse=True
        )

    return grade_data

def release_version(version):
    print(version)

def main():
    parser = argparse.ArgumentParser(description="WMCO Release Status Dashboard")
   
    return run_cli()

def run_cli():
    data = summarize_versions()

    headers = [
        "Version",
        "Phase",
        "End of Life",
        "WMCO Version",
        "Creation Date",
        "Grade"
    ]

    rows = []
    for v in data:
        rows.append([
            v["version"],
            v["current_phase"] or "EOL",
            v["end_of_life"].strftime("%Y-%m-%d") if v["end_of_life"] else "N/A",
            v["wmco_z_stream_version"],
            v["creation_date"],
            v["latest_grade"],
        ])

    col_widths = [
        max(len(str(row[i])) for row in [headers] + rows)
        for i in range(len(headers))
    ]

    def format_row(row):
        return "  ".join(
            str(cell).ljust(col_widths[i])
            for i, cell in enumerate(row)
        )

    print(format_row(headers))
    print(format_row(["-" * w for w in col_widths]))

    for row in rows:
        print(format_row(row))


if __name__ == "__main__":
    sys.exit(main())
