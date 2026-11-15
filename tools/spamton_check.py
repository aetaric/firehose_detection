#!/usr/bin/env python3
# /// script
# requires-python = ">=3.10"
# dependencies = ["torch", "transformers", "requests"]
# ///
"""Score a Bluesky post or profile with the tiagozip/spamton spam classifier.

Accepts either an AT URI or a bsky.app URL:

    at://did:plc:abc123/app.bsky.feed.post/3kabc
    at://handle.bsky.social/app.bsky.actor.profile/self
    https://bsky.app/profile/handle.bsky.social/post/3kabc
    https://bsky.app/profile/handle.bsky.social

Prints the spam probability as a percentage. Pass several URLs to score them
all with one model load. --text lets you score arbitrary text instead.

Run with uv (no install needed):  uv run tools/spamton_check.py <url>
Or:  pip install torch transformers requests && python tools/spamton_check.py <url>

Uses the GPU automatically if a CUDA or ROCm build of torch is installed;
otherwise runs on CPU, which is plenty for a 32M parameter model.
"""

import argparse
import json
import re
import sys
from urllib.parse import quote

import requests

MODEL = "tiagozip/spamton"
PUBLIC_API = "https://public.api.bsky.app/xrpc"
POST = "app.bsky.feed.post"
PROFILE = "app.bsky.actor.profile"

_BSKY_URL = re.compile(
    r"^https?://(?:www\.)?bsky\.app/profile/(?P<actor>[^/?#]+)(?:/post/(?P<rkey>[^/?#]+))?/?(?:[?#].*)?$"
)
_AT_URI = re.compile(r"^at://(?P<actor>[^/]+)(?:/(?P<collection>[^/]+)(?:/(?P<rkey>[^/?#]+))?)?$")


def parse_target(s: str) -> tuple[str, str, str]:
    """Return (actor, collection, rkey) for an AT URI or bsky.app URL."""
    if m := _BSKY_URL.match(s):
        if m["rkey"]:
            return m["actor"], POST, m["rkey"]
        return m["actor"], PROFILE, "self"
    if m := _AT_URI.match(s):
        collection = m["collection"] or PROFILE
        rkey = m["rkey"] or ("self" if collection == PROFILE else None)
        if rkey is None:
            raise ValueError(f"missing rkey in {s}")
        return m["actor"], collection, rkey
    raise ValueError(f"not an AT URI or bsky.app URL: {s}")


def resolve_did(actor: str) -> str:
    if actor.startswith("did:"):
        return actor
    r = requests.get(f"{PUBLIC_API}/com.atproto.identity.resolveHandle",
                     params={"handle": actor}, timeout=15)
    r.raise_for_status()
    return r.json()["did"]


def get_record(did: str, collection: str, rkey: str) -> dict:
    r = requests.get(f"{PUBLIC_API}/com.atproto.repo.getRecord",
                     params={"repo": did, "collection": collection, "rkey": rkey}, timeout=15)
    r.raise_for_status()
    return r.json()["value"]


def extract_text(collection: str, record: dict) -> str:
    """Pull the same text fields firehose-detection scans. Never touches media."""
    parts: list[str] = []
    if collection == POST:
        parts.append(record.get("text", ""))
        embed = record.get("embed") or {}
        # Unwrap recordWithMedia to get at the media half.
        media = embed.get("media", embed)
        for img in media.get("images", []) or []:
            if alt := img.get("alt"):
                parts.append(alt)
        if alt := media.get("alt"):  # video
            parts.append(alt)
        if ext := media.get("external"):
            parts.extend(filter(None, (ext.get("title"), ext.get("description"), ext.get("uri"))))
        for facet in record.get("facets", []) or []:
            for feat in facet.get("features", []) or []:
                if uri := feat.get("uri"):
                    parts.append(uri)
    elif collection == PROFILE:
        parts.extend(filter(None, (record.get("displayName"), record.get("description"),
                                   record.get("pronouns"), record.get("website"))))
    else:
        # Unknown collection: best effort over any string fields.
        parts.extend(v for v in record.values() if isinstance(v, str))
    return "\n".join(p for p in parts if p)


def load_model():
    import torch
    from transformers import AutoModelForSequenceClassification, AutoTokenizer

    # ROCm builds of torch expose AMD GPUs through the "cuda" device name.
    device = "cuda" if torch.cuda.is_available() else "cpu"
    tok = AutoTokenizer.from_pretrained(MODEL)
    model = AutoModelForSequenceClassification.from_pretrained(MODEL).eval().to(device)
    spam_idx = model.config.label2id.get("spam", 1)

    def score(text: str) -> float:
        enc = tok(text, truncation=True, max_length=512, return_tensors="pt").to(device)
        with torch.no_grad():
            probs = torch.softmax(model(**enc).logits, -1)[0]
        return probs[spam_idx].item() * 100

    return score


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("targets", nargs="*", help="AT URIs or bsky.app URLs")
    ap.add_argument("--text", action="append", default=[], help="score this literal text instead of/in addition to URLs")
    ap.add_argument("--show-text", action="store_true", help="print the text that was scored")
    ap.add_argument("--json", action="store_true", help="emit one JSON object per line")
    args = ap.parse_args()

    if not args.targets and not args.text:
        ap.error("give at least one URL or --text")

    # Fetch everything first so network errors surface before the slow model load.
    items: list[tuple[str, str]] = []
    for t in args.targets:
        try:
            actor, collection, rkey = parse_target(t)
            did = resolve_did(actor)
            record = get_record(did, collection, rkey)
        except (ValueError, requests.RequestException, KeyError) as e:
            print(f"{t}: error: {e}", file=sys.stderr)
            continue
        items.append((t, extract_text(collection, record)))
    for t in args.text:
        items.append(("--text", t))

    if not items:
        return 1

    score = load_model()
    for label, text in items:
        if not text.strip():
            print(f"{label}: no text to score", file=sys.stderr)
            continue
        pct = score(text)
        if args.json:
            print(json.dumps({"target": label, "spam_percent": round(pct, 2), "text": text}, ensure_ascii=False))
        else:
            print(f"{pct:6.2f}% spam  {label}")
            if args.show_text:
                for line in text.splitlines():
                    print(f"    | {line}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
